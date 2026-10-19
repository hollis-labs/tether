package turnrouting

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/messaging/channels"
)

// Cancellation starts subscriber teardown; channel closure acknowledges that its
// replay reads have finished. Keep those separate, as the real event bus does.
type controlledReplayBus struct {
	events.Bus
	canceled chan struct{}
	release  chan struct{}
	once     sync.Once
}

func (b *controlledReplayBus) Subscribe(context.Context, events.Filter) (<-chan events.Event, func(), error) {
	out := make(chan events.Event)
	go func() { <-b.canceled; <-b.release; close(out) }()
	return out, func() { b.once.Do(func() { close(b.canceled) }) }, nil
}

func TestRouterCloseJoinsOwnedSubscription(t *testing.T) {
	db, _ := database(t)
	bus := &controlledReplayBus{canceled: make(chan struct{}), release: make(chan struct{})}
	router := New(db, bus, channels.New(db, nil))
	if err := router.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	closed := make(chan struct{})
	go func() { router.Close(); close(closed) }()
	// Ensure the held subscriber is always released, even on assertion failure.
	defer func() { close(bus.release); <-closed }()
	select {
	case <-bus.canceled:
	case <-time.After(time.Second):
		t.Fatal("router did not cancel subscription")
	}
	select {
	case <-closed:
		t.Fatal("Close returned before subscriber replay finished")
	case <-time.After(25 * time.Millisecond):
	}
}
