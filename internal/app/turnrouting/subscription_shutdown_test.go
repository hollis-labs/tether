package turnrouting

import (
	"context"
	"errors"
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

type subscriptionStep struct {
	live   <-chan events.Event
	cancel func()
	err    error
}
type scriptedSubscriptionBus struct {
	events.Bus
	steps []subscriptionStep
	calls chan int
	next  int
}

func (b *scriptedSubscriptionBus) Subscribe(context.Context, events.Filter) (<-chan events.Event, func(), error) {
	i := b.next
	b.next++
	b.calls <- b.next
	if i >= len(b.steps) {
		return nil, nil, errors.New("unexpected subscription")
	}
	step := b.steps[i]
	return step.live, step.cancel, step.err
}

// Drive the router's existing ticker seam until the requested subscribe occurs.
func driveSubscription(t *testing.T, ticks chan time.Time, calls <-chan int, want int) {
	t.Helper()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for {
		select {
		case n := <-calls:
			if n == want {
				return
			}
		case ticks <- time.Now():
		case <-deadline.C:
			t.Fatalf("subscription %d never attempted", want)
		}
	}
}

func TestRouterCloseJoinsResubscribedWorker(t *testing.T) {
	db, _ := database(t)
	first := make(chan events.Event)
	close(first) // evicted before the first tick
	second := make(chan events.Event)
	canceled := make(chan struct{})
	release := make(chan struct{})
	var cancelOnce, releaseOnce sync.Once
	releaseWorker := func() { releaseOnce.Do(func() { close(release) }) }
	go func() { <-canceled; <-release; close(second) }()
	bus := &scriptedSubscriptionBus{calls: make(chan int, 8), steps: []subscriptionStep{
		{live: first, cancel: func() {}},
		{live: second, cancel: func() { cancelOnce.Do(func() { close(canceled) }) }},
	}}
	ticks := make(chan time.Time)
	router := New(db, bus, channels.New(db, nil))
	router.ticks = ticks
	if err := router.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { releaseWorker(); router.Close() })
	driveSubscription(t, ticks, bus.calls, 2)
	var callers sync.WaitGroup
	for range 4 {
		callers.Add(1)
		go func() { defer callers.Done(); router.Close() }()
	}
	closed := make(chan struct{})
	go func() { callers.Wait(); close(closed) }()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("current subscriber not canceled")
	}
	select {
	case <-closed:
		t.Fatal("Close joined old subscription instead of current replay")
	case <-time.After(25 * time.Millisecond):
	}
	releaseWorker()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close did not join released subscriber")
	}
	router.Close()
}

func TestRouterFailedResubscribeRetainsOwnedCancel(t *testing.T) {
	db, _ := database(t)
	first := make(chan events.Event)
	close(first)
	second := make(chan events.Event)
	canceled := make(chan struct{})
	var once sync.Once
	bus := &scriptedSubscriptionBus{calls: make(chan int, 8), steps: []subscriptionStep{
		{live: first, cancel: func() {}},
		{live: second, cancel: func() { once.Do(func() { close(canceled) }) }},
		{err: errors.New("resubscribe unavailable")},
	}}
	ticks := make(chan time.Time)
	router := New(db, bus, channels.New(db, nil))
	router.ticks = ticks
	if err := router.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(router.Close)
	driveSubscription(t, ticks, bus.calls, 2)
	close(second) // current worker is evicted; its cancel still belongs to router
	driveSubscription(t, ticks, bus.calls, 3)
	router.Close()
	router.Close()
	select {
	case <-canceled:
	default:
		t.Fatal("failed resubscribe discarded current subscription cancel")
	}
}
