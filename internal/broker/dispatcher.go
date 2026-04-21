package broker

import (
	"context"
	"sync"
)

// Dispatcher manages in-memory wait/notify for request/reply correlation.
// When a client posts a request envelope (via the API) it may open a
// Wait call with the server-assigned correlation_id. When a response
// envelope with that correlation_id is created, Deliver unblocks the
// waiter.
//
// Dispatcher state is ephemeral — if the daemon restarts, all in-flight
// waits are lost. Clients receive a context-canceled error and may retry.
type Dispatcher struct {
	mu      sync.Mutex
	waiters map[string]chan *Envelope
}

// NewDispatcher constructs an empty Dispatcher.
func NewDispatcher() *Dispatcher {
	return &Dispatcher{waiters: make(map[string]chan *Envelope)}
}

// Wait blocks until a response with the given correlationID is delivered
// or ctx is canceled. Returns the response envelope or a context error.
func (d *Dispatcher) Wait(ctx context.Context, correlationID string) (*Envelope, error) {
	ch := make(chan *Envelope, 1)

	d.mu.Lock()
	d.waiters[correlationID] = ch
	d.mu.Unlock()

	defer func() {
		d.mu.Lock()
		delete(d.waiters, correlationID)
		d.mu.Unlock()
	}()

	select {
	case env := <-ch:
		return env, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Deliver unblocks any Wait call registered for env.CorrelationID.
// If no waiter is registered the call is a no-op (the response arrived
// before the wait was registered, or the wait timed out).
func (d *Dispatcher) Deliver(env *Envelope) {
	d.mu.Lock()
	ch, ok := d.waiters[env.CorrelationID]
	d.mu.Unlock()
	if ok {
		ch <- env
	}
}
