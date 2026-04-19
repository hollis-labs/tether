package events

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
)

// BusOptions configures a Bus. Persister is required; the bus writes
// every published event through it before fanning out. SubBuffer is
// the per-subscriber channel capacity (default 1024). MaxConsecDrops
// controls slow-subscriber eviction: after this many consecutive
// drop-oldest events on a single subscriber, the bus closes its
// channel so the caller observes termination (default 64).
type BusOptions struct {
	Persister       Persister
	SubBuffer       int
	MaxConsecDrops  int64
}

// Publisher is the write half of the bus. Both runtime.Manager and
// the daemon depend on this narrow interface rather than the full
// Bus so they can't accidentally subscribe from a publisher seat.
type Publisher interface {
	Publish(ctx context.Context, e Event) error
}

// Bus is the pub/sub surface. Publish persists the event then fans it
// out to every matching subscriber. Subscribe returns a receive-only
// channel, a cancel func (idempotent), and an error; the channel is
// closed when the subscriber is cancelled or evicted.
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
)

type subscriber struct {
	id        int64
	filter    Filter
	history   []Event       // snapshot at Subscribe time; drained before live
	live      chan Event    // publisher → worker
	out       chan Event    // worker → user
	done      chan struct{} // closed on cancel/eviction; never re-opened
	closed    atomic.Bool
	drops     atomic.Int64
	consec    atomic.Int64
	maxConsec int64
}

// memBus is the in-memory Bus implementation. Persistence is
// delegated to Persister.
type memBus struct {
	persister      Persister
	subBuffer      int
	maxConsecDrops int64

	mu      sync.Mutex
	nextID  int64
	subs    map[int64]*subscriber
}

// NewBus constructs the default in-memory Bus.
func NewBus(opts BusOptions) Bus {
	if opts.SubBuffer <= 0 {
		opts.SubBuffer = defaultSubBuffer
	}
	if opts.MaxConsecDrops <= 0 {
		opts.MaxConsecDrops = defaultMaxConsecDrops
	}
	return &memBus{
		persister:      opts.Persister,
		subBuffer:      opts.SubBuffer,
		maxConsecDrops: opts.MaxConsecDrops,
		subs:           make(map[int64]*subscriber),
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

	b.mu.Lock()
	defer b.mu.Unlock()

	seq, at, err := b.persister.InsertEvent(e.Scope, e.SessionID, e.Kind, e.PayloadJSON)
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
// concurrently-cancelled subscriber's channel is never sent to after
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
// begins, with no gaps at the boundary. The replay snapshot and
// subscriber registration happen under the bus lock, so a Publish
// cannot slip an event in between.
func (b *memBus) Subscribe(ctx context.Context, f Filter) (<-chan Event, func(), error) {
	b.mu.Lock()

	// Always replay: EventsSince(0) returns the full history, which is
	// the right default for subscribers that want to catch up from the
	// start. Callers wanting live-only subscribe with Filter.SinceSeq
	// set to the current high-water mark.
	history, err := b.persister.EventsSince(f.SinceSeq)
	if err != nil {
		b.mu.Unlock()
		return nil, nil, err
	}

	b.nextID++
	s := &subscriber{
		id:        b.nextID,
		filter:    f,
		history:   history,
		live:      make(chan Event, b.subBuffer),
		out:       make(chan Event, b.subBuffer),
		done:      make(chan struct{}),
		maxConsec: b.maxConsecDrops,
	}
	b.subs[s.id] = s
	b.mu.Unlock()

	go b.runSubscriber(s)

	cancel := func() { b.evict(s) }
	return s.out, cancel, nil
}

// runSubscriber first drains the replay history snapshot, then
// drains the live channel, each applying the filter. On cancel or
// eviction (s.done closed) it exits, closes out, and deregisters.
func (b *memBus) runSubscriber(s *subscriber) {
	defer func() {
		close(s.out)
		b.mu.Lock()
		delete(b.subs, s.id)
		b.mu.Unlock()
	}()
	for _, e := range s.history {
		if !s.filter.matches(e) {
			continue
		}
		select {
		case <-s.done:
			return
		case s.out <- e:
		}
	}
	for {
		select {
		case <-s.done:
			return
		case e := <-s.live:
			if !s.filter.matches(e) {
				continue
			}
			select {
			case <-s.done:
				return
			case s.out <- e:
			}
		}
	}
}

// subscriberDropCount is an internal hook for tests.
func (b *memBus) subscriberDropCount() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	var total int64
	for _, s := range b.subs {
		total += s.drops.Load()
	}
	return total
}
