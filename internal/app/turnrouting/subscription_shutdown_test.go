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

func TestRouterRepeatedCloseAfterParentCancellation(t *testing.T) {
	db, _ := database(t)
	router := New(db, events.NewBus(events.BusOptions{Persister: db}), channels.New(db, nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := router.Start(ctx); err != nil {
		t.Fatal(err)
	}
	cancel()
	var callers sync.WaitGroup
	for range 4 {
		callers.Add(1)
		go func() { defer callers.Done(); router.Close() }()
	}
	joined := make(chan struct{})
	go func() { callers.Wait(); close(joined) }()
	select {
	case <-joined:
	case <-time.After(time.Second):
		t.Fatal("repeated Close deadlocked after parent cancellation")
	}
	router.Close()
	if router.Running() {
		t.Fatal("closed router still running")
	}
}
