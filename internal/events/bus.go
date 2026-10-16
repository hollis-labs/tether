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
	Persister      Persister
	SubBuffer      int
	MaxConsecDrops int64
	// ReplayPage bounds how many events one replay page loads when the
	// Persister is a PagedPersister (default 512).
	ReplayPage int
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
	ErrNoPersister = errors.New("events: bus requires a Persister")
	ErrScopeEmpty  = errors.New("events: Event.Scope is required")
)

const (
	defaultSubBuffer      = 1024
	defaultMaxConsecDrops = 64
	defaultReplayPage     = 512
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
	closed    atomic.Bool
	slow      atomic.Bool // evicted for lack of progress (not a plain cancel)
	drops     atomic.Int64
	signaled int64 // drops already reported to the subscriber
	consec    atomic.Int64
	maxConsec int64
}

// memBus is the in-memory Bus implementation. Persistence is
// delegated to Persister.
type memBus struct {
	persister      Persister
	paged          PagedPersister
	subBuffer      int
	maxConsecDrops int64
	replayPage     int

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
	paged, _ := opts.Persister.(PagedPersister)
	return &memBus{
		paged:          paged,
		replayPage:     opts.ReplayPage,
		persister:      opts.Persister,
		subBuffer:      opts.SubBuffer,
		maxConsecDrops: opts.MaxConsecDrops,
		subs:           make(map[int64]*subscriber),
		mu:             make(chan struct{}, 1),
	}
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
	if b.persister == nil {
		return ErrNoPersister
	}
	if e.Scope == "" {
		return ErrScopeEmpty
	}

	if err := b.lock(ctx); err != nil {
		return err
	}
	defer b.unlock()

	var seq int64
	var at time.Time
	var err error
	if persister, ok := b.persister.(ContextPersister); ok {
		seq, at, err = persister.InsertEventContext(ctx, e.Scope, e.SessionID, e.Kind, e.PayloadJSON)
	} else {
		seq, at, err = b.persister.InsertEvent(e.Scope, e.SessionID, e.Kind, e.PayloadJSON)
	}
	if err != nil {
		return err
	}
	e.Seq = seq
	e.At = at

	for _, s := range b.subs {
		if s.filter.matches(e) {
			b.pushTo(s, e)
		}
	}
	return nil
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
	// Buffer full: drop the oldest buffered event, then push.
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
	s.drops.Add(1)
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
		id:     b.nextID,
		filter: f,
		// Replay reads outlive the Subscribe call; the subscriber's lifetime is
		// its cancel func and eviction, as before, not the caller's context.
		ctx:       context.WithoutCancel(ctx),
		live:      make(chan Event, b.subBuffer),
		out:       make(chan Event, b.subBuffer),
		done:      make(chan struct{}),
		maxConsec: b.maxConsecDrops,
		cursor:    f.SinceSeq,
	}
	if b.paged != nil {
		earliest, head, err := b.paged.StreamBoundsContext(ctx)
		if err != nil {
			b.unlock()
			return nil, nil, err
		}
		s.paged = true
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
	for {
		_, head, err := b.paged.StreamBoundsContext(s.ctx)
		if err != nil {
			return b.replayFailed(s, err)
		}
		if s.cursor >= head {
			return true
		}
		for s.cursor < head {
			page, err := b.paged.EventsPageContext(s.ctx, s.filter, s.cursor, head, b.replayPage)
			if err != nil {
				return b.replayFailed(s, err)
			}
			for _, e := range page {
				if !s.send(e) {
					return false
				}
				s.cursor = e.Seq
			}
			if len(page) < b.replayPage {
				s.cursor = head
			}
		}
	}
}

func (b *memBus) replayFailed(s *subscriber, err error) bool {
	select {
	case <-s.done:
		return false
	default:
	}
	log.Printf("events: replay for subscriber %d failed: %v", s.id, err)
	s.gap(GapPayload{Reason: "replay_error", LastSeq: s.cursor})
	b.evict(s)
	return false
}

// runSubscriber first catches up on history, then follows the live channel,
// applying the filter. On cancel or eviction (s.done closed) it exits, closes
// out, and deregisters. An evicted subscriber is told why before the close.
func (b *memBus) runSubscriber(s *subscriber) {
	defer func() {
		if s.slow.Load() {
			// Make room if the consumer left out full: the final signal matters
			// more than the oldest buffered event.
			g := GapPayload{Reason: "evicted", Dropped: s.drops.Load() - s.signaled, LastSeq: s.cursor}
			raw, _ := json.Marshal(g)
			ev := Event{Seq: s.cursor, At: time.Now().UTC(), Scope: ScopeDaemon, Kind: KindStreamGap, PayloadJSON: string(raw)}
			select {
			case s.out <- ev:
			default:
				select {
				case <-s.out:
				default:
				}
				select {
				case s.out <- ev:
				default:
				}
			}
		}
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
		covered := s.drops.Load() // live drops before this read are inside the catch-up
		if !b.catchUp(s) {
			return
		}
		s.signaled = covered
	} else {
		for _, e := range s.history {
			if !s.filter.matches(e) {
				continue
			}
			if !s.send(e) {
				return
			}
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
					covered := s.drops.Load()
					if !b.catchUp(s) {
						return
					}
					s.signaled = covered
					continue
				}
				if !s.gap(GapPayload{Reason: "overrun", Dropped: dropped, LastSeq: s.cursor}) {
					return
				}
			}
			s.cursor = e.Seq
			if !s.filter.matches(e) {
				continue
			}
			if !s.send(e) {
				return
			}
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
