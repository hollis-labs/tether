package events

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"sync/atomic"
	"time"
)

// BusOptions configures a Bus. Persister is required; the bus writes
// every published event through it before fanning out. SubBuffer is
// the per-subscriber channel capacity (default 1024). MaxConsecDrops
// controls slow-subscriber eviction: after this many consecutive
// drop-oldest events on a single subscriber, the bus closes its
// channel so the caller observes termination (default 64).
type BusOptions struct {
	// Pipeline optionally runs shared sanitation/classification after stamping.
	Pipeline       *Pipeline
	Persister      Persister
	SubBuffer      int
	MaxConsecDrops int64
	// ReplayPage bounds how many events one replay page loads when the
	// Persister is a PagedPersister (default 512).
	ReplayPage int
	// OutboxSweep is how often the outbox drainer looks for committed pending
	// rows nobody kicked it about (default 2s).
	OutboxSweep time.Duration
	// StallTimeout is how long a paged subscriber's replay or recovery may sit
	// blocked on a consumer that is not reading before the subscriber is
	// evicted (default 30s). Live drops do not evict during replay.
	StallTimeout time.Duration
	// Stages stamp every event before it is written, in order (see Stage). Nil
	// means the bus writes the event exactly as the producer built it.
	Stages []Stage
	// Observers see each Publish'd event after it is persisted and fanned out.
	Observers []Observer
	// OnStageError receives a stage failure; the event is still published.
	// Default: log it.
	OnStageError func(*StageError)
}

// Publisher is the write half of the bus. Lifecycle producers (the
// agentsessions.Manager event sink in app/sinks.go, the daemon, the
// broker service) depend on this narrow interface rather than the
// full Bus so they can't accidentally subscribe from a publisher seat.
type Publisher interface {
	Publish(ctx context.Context, e Event) error
}

// Bus is the pub/sub surface. Publish persists the event then fans it
// out to every matching subscriber. Subscribe returns a receive-only
// channel, a cancel func (idempotent), and an error; the channel is
// closed when the subscriber is canceled or evicted.
type Bus interface {
	Publisher
	Subscribe(ctx context.Context, f Filter) (<-chan Event, func(), error)
}

// Errors returned by the bus.
var (
	ErrNoPersister   = errors.New("events: bus requires a Persister")
	ErrNoTxPersister = errors.New("events: this bus's Persister cannot write on a caller's transaction")
	ErrScopeEmpty    = errors.New("events: Event.Scope is required")
)

const (
	defaultSubBuffer      = 1024
	defaultMaxConsecDrops = 64
	defaultReplayPage     = 512
	defaultStallTimeout   = 30 * time.Second
)

type subscriber struct {
	id     int64
	filter Filter
	ctx    context.Context
	// history is the one-shot replay snapshot of a non-paged Persister. A
	// paged subscriber has none: it reads pages from the log outside the bus
	// lock, from cursor up to the head, then follows live.
	history []Event
	paged   bool
	cursor  int64 // last sequence processed (delivered or filtered out)
	// trimmedFrom/trimmedEarliest/trimmedHead describe history the subscriber
	// asked for that no longer exists; announced before replay.
	trimmed         bool
	trimmedEarliest int64
	trimmedHead     int64

	live      chan Event    // publisher → worker
	out       chan Event    // worker → user
	done      chan struct{} // closed on cancel/eviction; never re-opened
	stop      context.CancelFunc
	catching  atomic.Bool // the worker is replaying or recovering from the log
	closed    atomic.Bool
	slow      atomic.Bool // evicted for lack of progress (not a plain cancel)
	drops     atomic.Int64
	signaled  int64 // drops already reported to the subscriber
	consec    atomic.Int64
	maxConsec int64
}

// memBus is the in-memory Bus implementation. Persistence is
// delegated to Persister.
type memBus struct {
	pipeline       *Pipeline
	persister      Persister
	paged          PagedPersister
	txp            TxPersister
	idp            EventIDPersister
	envp           EnvelopePersister
	outboxGap      time.Duration
	dirty          atomic.Bool   // committed outbox rows may be waiting
	kick           chan struct{} // wakes the drainer
	subBuffer      int
	maxConsecDrops int64
	replayPage     int
	stallTimeout   time.Duration
	stages         []Stage
	observers      []Observer
	onStageError   func(*StageError)

	published atomic.Int64 // newest sequence this bus persisted (high_water for non-paged persisters)

	mu     chan struct{}
	nextID int64
	subs   map[int64]*subscriber
}

// NewBus constructs the default in-memory Bus.
func NewBus(opts BusOptions) Bus {
	if opts.SubBuffer <= 0 {
		opts.SubBuffer = defaultSubBuffer
	}
	if opts.MaxConsecDrops <= 0 {
		opts.MaxConsecDrops = defaultMaxConsecDrops
	}
	if opts.ReplayPage <= 0 {
		opts.ReplayPage = defaultReplayPage
	}
	if opts.StallTimeout <= 0 {
		opts.StallTimeout = defaultStallTimeout
	}
	paged, _ := opts.Persister.(PagedPersister)
	txp, _ := opts.Persister.(TxPersister)
	idp, _ := opts.Persister.(EventIDPersister)
	envp, _ := opts.Persister.(EnvelopePersister)
	if opts.OutboxSweep <= 0 {
		opts.OutboxSweep = defaultOutboxGap
	}
	if opts.OnStageError == nil {
		opts.OnStageError = defaultOnStageError
	}
	b := &memBus{
		pipeline:       opts.Pipeline,
		stages:         append([]Stage(nil), opts.Stages...),
		observers:      append([]Observer(nil), opts.Observers...),
		onStageError:   opts.OnStageError,
		paged:          paged,
		txp:            txp,
		idp:            idp,
		envp:           envp,
		outboxGap:      opts.OutboxSweep,
		kick:           make(chan struct{}, 1),
		replayPage:     opts.ReplayPage,
		stallTimeout:   opts.StallTimeout,
		persister:      opts.Persister,
		subBuffer:      opts.SubBuffer,
		maxConsecDrops: opts.MaxConsecDrops,
		subs:           make(map[int64]*subscriber),
		mu:             make(chan struct{}, 1),
	}
	// Rows a crashed process left pending can be overtaken before StartOutbox
	// runs: start dirty so the first Publish drains them.
	b.dirty.Store(txp != nil)
	return b
}

// Publish persists the event, assigns its Seq/At, and fans out to
// every matching subscriber. Persistence errors are returned;
// fan-out is best-effort and non-blocking (slow subscribers drop
// oldest rather than block the publisher).
//
// The bus lock spans both the persister insert and the fan-out to
// guarantee ordering against Subscribe's replay window: no
// concurrent Publish can slip a new event between a subscriber's
// history snapshot and its live-stream registration.
func (b *memBus) Publish(ctx context.Context, e Event) error {
	stored, err := b.publish(ctx, e)
	if err == nil && len(b.observers) > 0 {
		b.observe(ctx, stored)
	}
	return err
}

// publish persists and fans out e, returning the stored event.
func (b *memBus) publish(ctx context.Context, e Event) (Event, error) {
	if b.persister == nil {
		return Event{}, ErrNoPersister
	}
	if e.Scope == "" {
		return Event{}, ErrScopeEmpty
	}
	// Stamp before the lock: a stage may read context or a cache, and must not
	// delay other publishers.
	e = b.stamp(ctx, e, false)

	if err := b.lock(ctx); err != nil {
		return Event{}, err
	}
	defer b.unlock()

	// Rows committed on a caller's transaction have lower Seq than this event and
	// must be delivered first, or a cursor subscriber (which advances past every
	// Seq it sees) would drop them for good. Drain them before the insert when the
	// outbox is dirty, so a failure stops here with nothing persisted...
	if b.dirty.Load() {
		if err := b.drainLocked(ctx, 0); err != nil {
			return Event{}, err
		}
	}

	var seq int64
	var at time.Time
	var err error
	switch {
	case b.envp != nil:
		// The whole envelope is written with the row; the log assigns the id,
		// the time and recorded_at.
		var stored Event
		stored, err = b.envp.InsertEventEnvelope(ctx, e)
		if err == nil {
			seq, at, e = stored.Seq, stored.At, stored
		}
	case b.idp != nil:
		seq, at, e.EventID, err = b.idp.InsertEventIDContext(ctx, e.Scope, e.SessionID, e.Kind, e.PayloadJSON)
	default:
		if persister, ok := b.persister.(ContextPersister); ok {
			seq, at, err = persister.InsertEventContext(ctx, e.Scope, e.SessionID, e.Kind, e.PayloadJSON)
		} else {
			seq, at, err = b.persister.InsertEvent(e.Scope, e.SessionID, e.Kind, e.PayloadJSON)
		}
	}
	if err != nil {
		return Event{}, err
	}
	e.Seq = seq
	e.At = at
	b.published.Store(seq)
	// ...and again after it, unconditionally: this insert may have waited for the
	// connection while a caller's transaction wrote and committed a pending row
	// with a lower Seq than ours (the dirty check above ran before that). The probe
	// reads a partial index that is empty unless something is pending. If it fails
	// the event is persisted but is not fanned out above a pending row.
	if err := b.drainLocked(ctx, seq); err != nil {
		return Event{}, err
	}
	b.fanout(e)
	return e, nil
}

// fanout delivers one persisted event to every matching subscriber. The bus
// lock must be held.
func (b *memBus) fanout(e Event) {
	for _, s := range b.subs {
		if s.filter.matches(e) {
			b.pushTo(s, e)
		}
	}
}

// pushTo delivers an event to a single subscriber with drop-oldest
// semantics on a full buffer. After maxConsecDrops consecutive drops,
// the subscriber is evicted. The select on s.done ensures a
// concurrently-canceled subscriber's channel is never sent to after
// shutdown — live is never closed, so there is no close-vs-send race.
func (b *memBus) pushTo(s *subscriber, e Event) {
	select {
	case <-s.done:
		return
	default:
	}
	select {
	case <-s.done:
		return
	case s.live <- e:
		s.consec.Store(0)
		return
	default:
	}
	// Buffer full: drop the oldest buffered event, then push. The drop is
	// counted BEFORE the oldest event leaves the buffer: a worker that then
	// reads the next event must already see the overrun, or the removed event
	// would be missing from a recovery that starts after the one it just read.
	s.drops.Add(1)
	select {
	case <-s.live:
	default:
	}
	select {
	case <-s.done:
		return
	case s.live <- e:
	default:
	}
	if s.catching.Load() {
		// The worker is busy re-reading the durable log, not stuck: everything
		// dropped now is covered by that read. A consumer that really stopped
		// reading is evicted by the send stall timeout instead.
		return
	}
	if s.consec.Add(1) >= s.maxConsec {
		s.slow.Store(true)
		b.evict(s)
	}
}

// evict signals the subscriber to shut down by closing its done
// channel. The worker goroutine observes done, exits, closes out,
// and deregisters from subs. Idempotent.
func (b *memBus) evict(s *subscriber) {
	if s.closed.CompareAndSwap(false, true) {
		close(s.done)
		if s.stop != nil {
			s.stop() // abandon an in-flight replay read
		}
	}
}

// Subscribe registers a new subscriber. If f.SinceSeq > 0, the bus
// replays persisted events with Seq > SinceSeq before live delivery
// begins, with no gaps at the boundary. With a PagedPersister the replay is
// read in bounded pages by the subscriber's own goroutine, never under the bus
// lock, and a subscriber that falls behind is recovered from the durable log
// and told so with a KindStreamGap event instead of losing events or being
// closed silently. Without one the full snapshot is taken under the lock.
func (b *memBus) Subscribe(ctx context.Context, f Filter) (<-chan Event, func(), error) {
	if err := b.lock(ctx); err != nil {
		return nil, nil, err
	}

	b.nextID++
	s := &subscriber{
		id:        b.nextID,
		filter:    f,
		live:      make(chan Event, b.subBuffer),
		out:       make(chan Event, b.subBuffer),
		done:      make(chan struct{}),
		maxConsec: b.maxConsecDrops,
		cursor:    f.SinceSeq,
	}
	// Replay reads outlive the Subscribe call; the subscriber's lifetime is its
	// cancel func and eviction, as before, not the caller's context. They stop
	// when the subscriber does.
	s.ctx, s.stop = context.WithCancel(context.WithoutCancel(ctx))
	if b.paged != nil {
		earliest, head, err := b.paged.StreamBoundsContext(ctx)
		if err != nil {
			b.unlock()
			return nil, nil, err
		}
		s.paged = true
		// The worker starts by re-reading the log; live drops before it gets
		// going are covered by that read and must not evict it.
		s.catching.Store(true)
		if f.SinceSeq > 0 && earliest > 0 && f.SinceSeq+1 < earliest {
			s.trimmed, s.trimmedEarliest, s.trimmedHead = true, earliest, head
			s.cursor = earliest - 1
		}
	} else {
		// Always replay: EventsSince(0) returns the full history, which is
		// the right default for subscribers that want to catch up from the
		// start. Callers wanting live-only subscribe with Filter.SinceSeq
		// set to the current high-water mark.
		var history []Event
		var err error
		if persister, ok := b.persister.(ContextPersister); ok {
			history, err = persister.EventsSinceContext(ctx, f.SinceSeq)
		} else {
			history, err = b.persister.EventsSince(f.SinceSeq)
		}
		if err != nil {
			b.unlock()
			return nil, nil, err
		}
		s.history = history
	}
	b.subs[s.id] = s
	b.unlock()

	go b.runSubscriber(s)

	cancel := func() { b.evict(s) }
	return s.out, cancel, nil
}

// send delivers one event to the subscriber, or reports false if it went away.
func (s *subscriber) send(e Event) bool {
	select {
	case <-s.done:
		return false
	case s.out <- e:
		return true
	}
}

// sendStalled is send for the replay/recovery path: a consumer that does not
// take an event for stall is evicted (the live buffer's consecutive-drop count
// does not apply while the worker is re-reading the log).
func (b *memBus) sendStalled(s *subscriber, e Event) bool {
	select {
	case <-s.done:
		return false
	case s.out <- e:
		return true
	default:
	}
	t := time.NewTimer(b.stallTimeout)
	defer t.Stop()
	select {
	case <-s.done:
		return false
	case s.out <- e:
		return true
	case <-t.C:
		s.slow.Store(true)
		b.evict(s)
		return false
	}
}

// gap delivers a KindStreamGap signal.
func (s *subscriber) gap(p GapPayload) bool {
	raw, _ := json.Marshal(p)
	return s.send(Event{Seq: p.LastSeq, At: time.Now().UTC(), Scope: ScopeDaemon, Kind: KindStreamGap, PayloadJSON: string(raw)})
}

// catchUp reads the durable log from the cursor to the head in bounded pages.
// It loops until the head stops moving, so events published (or dropped from
// the live buffer) while it ran are covered. It returns false if the
// subscriber went away or the log could not be read (a gap was signaled).
func (b *memBus) catchUp(s *subscriber) bool {
	s.catching.Store(true)
	defer s.catching.Store(false)
	for {
		// Drops before this bounds read are inside this catch-up. Bounds are
		// re-read for every page: the head moves while we replay, and so can the
		// oldest row (a trim mid-replay is announced, not skipped).
		covered := s.drops.Load()
		earliest, head, err := b.paged.StreamBoundsContext(s.ctx)
		if err != nil {
			return b.replayFailed(s, err)
		}
		if s.cursor > 0 && earliest > 0 && s.cursor+1 < earliest {
			if !s.gap(GapPayload{Reason: "trimmed", LastSeq: s.cursor, EarliestAvailable: earliest, HighWater: head}) {
				return false
			}
			s.cursor = earliest - 1
		}
		if s.cursor >= head {
			s.signaled = covered
			return true
		}
		page, err := b.paged.EventsPageContext(s.ctx, s.filter, s.cursor, head, b.replayPage)
		if err != nil {
			return b.replayFailed(s, err)
		}
		for _, e := range page {
			if s.filter.matches(e) && !b.sendStalled(s, e) {
				return false
			}
			s.cursor = e.Seq
		}
		if len(page) < b.replayPage {
			s.cursor = head
		}
	}
}

// bounds reports what the log holds for a gap frame; it is best effort and
// never blocks a closing subscriber for long.
func (b *memBus) bounds(s *subscriber) (earliest, head int64) {
	if b.paged != nil {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(s.ctx), 2*time.Second)
		defer cancel()
		if lo, hi, err := b.paged.StreamBoundsContext(ctx); err == nil {
			return lo, hi
		}
	}
	return 0, b.published.Load()
}

// gapAt sends a gap frame carrying the log bounds.
func (b *memBus) gapAt(s *subscriber, p GapPayload) bool {
	p.EarliestAvailable, p.HighWater = b.bounds(s)
	return s.gap(p)
}

func (b *memBus) replayFailed(s *subscriber, err error) bool {
	select {
	case <-s.done:
		return false
	default:
	}
	log.Printf("events: replay for subscriber %d failed: %v", s.id, err)
	b.gapAt(s, GapPayload{Reason: "replay_error", LastSeq: s.cursor})
	b.evict(s)
	return false
}

// runSubscriber first catches up on history, then follows the live channel,
// applying the filter. On cancel or eviction (s.done closed) it exits, closes
// out, and deregisters. An evicted subscriber is told why before the close.
func (b *memBus) runSubscriber(s *subscriber) {
	defer func() {
		if s.slow.Load() {
			// Tell the consumer why it is being closed. If it left out full, the
			// signal matters more than the buffered events: discard them and
			// resume the cursor just before the first discarded one, so a
			// resume from last_seq loses and duplicates nothing.
			g := GapPayload{Reason: "evicted", Dropped: s.drops.Load() - s.signaled, LastSeq: s.cursor}
			g.EarliestAvailable, g.HighWater = b.bounds(s)
			send := func() bool {
				raw, _ := json.Marshal(g)
				select {
				case s.out <- Event{Seq: g.LastSeq, At: time.Now().UTC(), Scope: ScopeDaemon, Kind: KindStreamGap, PayloadJSON: string(raw)}:
					return true
				default:
					return false
				}
			}
			if !send() {
				first := int64(0)
			drain:
				for {
					select {
					case d := <-s.out:
						if d.Kind != KindStreamGap && (first == 0 || d.Seq < first) {
							first = d.Seq
						}
					default:
						break drain
					}
				}
				if first > 0 && first-1 < g.LastSeq {
					g.LastSeq = first - 1
				}
				send()
			}
		}
		s.stop()
		close(s.out)
		b.mu <- struct{}{}
		delete(b.subs, s.id)
		b.unlock()
	}()
	if s.trimmed {
		if !s.gap(GapPayload{Reason: "trimmed", LastSeq: s.cursor, EarliestAvailable: s.trimmedEarliest, HighWater: s.trimmedHead}) {
			return
		}
	}
	if s.paged {
		if !b.catchUp(s) {
			return
		}
	} else {
		for _, e := range s.history {
			if s.filter.matches(e) && !s.send(e) {
				return
			}
			s.cursor = e.Seq
		}
	}
	for {
		select {
		case <-s.done:
			return
		case e := <-s.live:
			if s.paged && e.Seq > 0 && e.Seq <= s.cursor {
				continue // already delivered by catch-up
			}
			if dropped := s.drops.Load() - s.signaled; dropped > 0 {
				s.signaled = s.drops.Load()
				if s.paged {
					// Recover from the durable log: events are persisted before
					// they are fanned out, so nothing the buffer dropped is lost.
					for len(s.live) > 0 {
						<-s.live
					}
					earliest, head, _ := b.paged.StreamBoundsContext(s.ctx)
					if !s.gap(GapPayload{Reason: "overrun", Dropped: dropped, LastSeq: s.cursor, EarliestAvailable: earliest, HighWater: head, Recovered: true}) {
						return
					}
					if !b.catchUp(s) {
						return
					}
					continue
				}
				if !b.gapAt(s, GapPayload{Reason: "overrun", Dropped: dropped, LastSeq: s.cursor}) {
					return
				}
			}
			if !s.filter.matches(e) {
				s.cursor = e.Seq
				continue
			}
			if !s.send(e) {
				return // not delivered: the cursor must not claim it was
			}
			s.cursor = e.Seq
		}
	}
}

// subscriberDropCount is an internal hook for tests.
func (b *memBus) subscriberDropCount() int64 {
	b.mu <- struct{}{}
	defer b.unlock()
	var total int64
	for _, s := range b.subs {
		total += s.drops.Load()
	}
	return total
}

// Waiting for another producer is bounded too; a SQL deadline alone would leave
// an event reader stuck behind a producer holding the bus lock.
func (b *memBus) lock(ctx context.Context) error {
	select {
	case b.mu <- struct{}{}:
		if err := ctx.Err(); err != nil {
			b.unlock()
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (b *memBus) unlock() { <-b.mu }
