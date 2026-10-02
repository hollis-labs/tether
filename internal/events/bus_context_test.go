package events

import (
	"context"
	"errors"
	"testing"
	"time"
)

type blockingContextPersister struct {
	fakePersister
	entered chan struct{}
}

func (p *blockingContextPersister) InsertEventContext(ctx context.Context, _ Scope, _ string, _ string, _ string) (int64, time.Time, error) {
	close(p.entered)
	<-ctx.Done()
	return 0, time.Time{}, ctx.Err()
}
func (p *blockingContextPersister) EventsSinceContext(ctx context.Context, seq int64) ([]Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return p.EventsSince(seq)
}
func TestBusPublishCancellationBoundsPersistenceAndGateWait(t *testing.T) {
	persister := &blockingContextPersister{entered: make(chan struct{})}
	bus := NewBus(BusOptions{Persister: persister})
	firstCtx, cancelFirst := context.WithCancel(context.Background())
	defer cancelFirst()
	first := make(chan error, 1)
	go func() { first <- bus.Publish(firstCtx, Event{Scope: ScopeSession, Kind: "first"}) }()
	select {
	case <-persister.entered:
	case <-time.After(time.Second):
		t.Fatal("context persister not called")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	second := make(chan error, 1)
	go func() { second <- bus.Publish(ctx, Event{Scope: ScopeSession, Kind: "second"}) }()
	select {
	case err := <-second:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled writer stuck behind bus gate")
	}
	cancelFirst()
	select {
	case err := <-first:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("persistence ignored cancellation")
	}
}
