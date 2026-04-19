package runtime

import (
	"context"
	"io"
	"sync"
)

// Default ring/fanout sizing. 64 KiB of scrollback is enough for most
// short-term replay without taking meaningful memory per session. Subscriber
// channel depth of 64 chunks absorbs brief consumer slowness before the
// broker starts dropping. Tuneable in a future sprint.
const (
	defaultRingBytes       = 64 * 1024
	defaultSubscriberDepth = 64
)

// attachBroker multiplexes PTY output from a single producer (the session's
// copy goroutine) to zero or more subscribers. It is io.Writer so it drops
// into the existing MultiWriter pipeline, and it owns a bounded replay ring
// buffer so new subscribers see recent history before the live stream.
//
// Invariants:
//   - Write never blocks on a slow subscriber. If a subscriber's channel is
//     full, the chunk is dropped for that subscriber (other subscribers are
//     unaffected). DroppedBytes counts these bytes per broker.
//   - close is idempotent. After close, Write returns io.ErrClosedPipe;
//     new subscribe calls return the replay and a channel that is already
//     closed (so callers can still drain recent history, then exit).
//   - All subscriber channels are closed exactly once — either on individual
//     cancel() or on broker close(), whichever comes first.
type attachBroker struct {
	ringCap  int
	chanCap  int
	dropCnt  int64
	mu       sync.Mutex
	ring     []byte
	subs     map[int]chan []byte
	subNext  int
	closed   bool
	closeErr error
}

func newAttachBroker(ringBytes, subscriberDepth int) *attachBroker {
	if ringBytes <= 0 {
		ringBytes = defaultRingBytes
	}
	if subscriberDepth <= 0 {
		subscriberDepth = defaultSubscriberDepth
	}
	return &attachBroker{
		ringCap: ringBytes,
		chanCap: subscriberDepth,
		ring:    make([]byte, 0, ringBytes),
		subs:    map[int]chan []byte{},
	}
}

// Write appends p to the replay ring (trimming the oldest bytes if the ring
// would overflow) and publishes a copy to every live subscriber. If a
// subscriber's channel is full the chunk is dropped for that subscriber and
// the dropped-byte counter is incremented.
func (b *attachBroker) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return 0, io.ErrClosedPipe
	}
	b.appendRing(p)

	// Copy once, share with every subscriber. Channels hold read-only views.
	cp := make([]byte, len(p))
	copy(cp, p)
	for _, ch := range b.subs {
		select {
		case ch <- cp:
		default:
			b.dropCnt += int64(len(cp))
		}
	}
	b.mu.Unlock()
	return len(p), nil
}

// appendRing extends b.ring with p, keeping only the trailing ringCap bytes.
// Caller holds b.mu.
func (b *attachBroker) appendRing(p []byte) {
	if len(p) >= b.ringCap {
		// Only the trailing ringCap bytes survive.
		b.ring = append(b.ring[:0], p[len(p)-b.ringCap:]...)
		return
	}
	spare := b.ringCap - len(b.ring)
	if len(p) > spare {
		drop := len(p) - spare
		b.ring = b.ring[drop:]
	}
	b.ring = append(b.ring, p...)
}

// subscribe returns (replay, live, cancel):
//   - replay: a snapshot of the current ring contents (safe to pass to w.Write)
//   - live:   a channel that receives future Write payloads
//   - cancel: idempotent function that unregisters and closes live
//
// After the broker is closed, subscribe still returns the current replay
// along with a channel that is already closed, so late subscribers drain
// history and then exit cleanly.
func (b *attachBroker) subscribe(depth int) (replay []byte, ch <-chan []byte, cancel func()) {
	if depth <= 0 {
		depth = b.chanCap
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	replay = make([]byte, len(b.ring))
	copy(replay, b.ring)

	out := make(chan []byte, depth)
	if b.closed {
		close(out)
		return replay, out, func() {}
	}

	id := b.subNext
	b.subNext++
	b.subs[id] = out

	var once sync.Once
	cancel = func() {
		once.Do(func() {
			b.mu.Lock()
			if c, ok := b.subs[id]; ok {
				delete(b.subs, id)
				close(c)
			}
			b.mu.Unlock()
		})
	}
	return replay, out, cancel
}

// close tears down the broker: closes every subscriber channel, marks the
// broker closed, and causes future Write calls to return io.ErrClosedPipe.
// Idempotent.
func (b *attachBroker) close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for id, ch := range b.subs {
		close(ch)
		delete(b.subs, id)
	}
}

// subscriberCount reports the number of currently-subscribed clients.
func (b *attachBroker) subscriberCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}

// droppedBytes reports the cumulative count of bytes dropped due to slow
// subscribers. Intended for observability; not exposed on the runtime API yet.
func (b *attachBroker) droppedBytes() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.dropCnt
}

// copyStream pumps a broker subscription to w until ctx is cancelled or the
// broker closes. Used by Manager.Attach so the bookkeeping lives in one place.
func copyStream(ctx context.Context, w io.Writer, replay []byte, ch <-chan []byte) error {
	if len(replay) > 0 {
		if _, err := w.Write(replay); err != nil {
			return err
		}
	}
	for {
		select {
		case chunk, ok := <-ch:
			if !ok {
				return nil
			}
			if _, err := w.Write(chunk); err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
