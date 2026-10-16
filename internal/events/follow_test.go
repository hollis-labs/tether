package events

import (
	"context"
	"sync"
	"testing"
	"time"
)

func fastBackoff(t *testing.T) {
	t.Helper()
	oldMin, oldMax := FollowBackoffFloor, FollowBackoffCeiling
	FollowBackoffFloor, FollowBackoffCeiling = 5*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { FollowBackoffFloor, FollowBackoffCeiling = oldMin, oldMax })
}

type collector struct {
	mu   sync.Mutex
	seqs []int64
	gaps int
}

func (c *collector) handle(e Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e.Kind == KindStreamGap {
		c.gaps++
		return
	}
	c.seqs = append(c.seqs, e.Seq)
}

func (c *collector) wait(t *testing.T, n int) []int64 {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		got := append([]int64(nil), c.seqs...)
		c.mu.Unlock()
		if len(got) >= n {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	t.Fatalf("got %d of %d events: %v", len(c.seqs), n, c.seqs)
	return nil
}

// A follower that stalls is evicted by the bus, then resumes from its cursor:
// every event arrives exactly once, in order, and the handler never sees a gap frame.
func TestFollow_ResumesFromItsCursorAfterEviction(t *testing.T) {
	fastBackoff(t)
	p := &pagedFake{}
	bus := NewBus(BusOptions{Persister: p, SubBuffer: 1, MaxConsecDrops: 2, ReplayPage: 7})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	release := make(chan struct{})
	var once sync.Once
	c := &collector{}
	FollowAsync(ctx, bus, Filter{}, FollowOptions{Name: "test"}, func(e Event) {
		c.handle(e)
		once.Do(func() { <-release }) // stall on the first event
	})
	for i := 0; i < 60; i++ {
		if err := bus.Publish(ctx, Event{Scope: ScopeSession, SessionID: "s", Kind: "k"}); err != nil {
			t.Fatal(err)
		}
	}
	close(release)
	_ = c.wait(t, 60)
	time.Sleep(50 * time.Millisecond) // a duplicate would show up late
	c.mu.Lock()
	got := append([]int64(nil), c.seqs...)
	c.mu.Unlock()
	if len(got) != 60 {
		t.Fatalf("%d events delivered, want exactly 60: %v", len(got), got)
	}
	for i, s := range got {
		if s != int64(i+1) {
			t.Fatalf("events duplicated, lost or reordered after resubscribe: %v", got)
		}
	}
	c.mu.Lock()
	gaps := c.gaps
	c.mu.Unlock()
	if gaps != 0 {
		t.Fatal("gap frames reached the handler")
	}
}

// LiveOnly does not replay history, and FollowAsync guarantees the follower is
// registered before it returns.
func TestFollow_LiveOnlySkipsHistoryAndIsRegisteredOnReturn(t *testing.T) {
	p := &fakePersister{}
	bus := NewBus(BusOptions{Persister: p})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for i := 0; i < 10; i++ {
		_ = bus.Publish(ctx, Event{Scope: ScopeSession, Kind: "old"})
	}
	c := &collector{}
	FollowAsync(ctx, bus, Filter{}, FollowOptions{LiveOnly: true}, c.handle)
	if err := bus.Publish(ctx, Event{Scope: ScopeSession, Kind: "new"}); err != nil {
		t.Fatal(err)
	}
	got := c.wait(t, 1)
	time.Sleep(50 * time.Millisecond)
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.seqs) != 1 || got[0] != 11 {
		t.Fatalf("live-only follower saw %v, want only seq 11", c.seqs)
	}
}

// Canceling the context ends Follow.
func TestFollow_StopsWhenTheContextIsDone(t *testing.T) {
	bus := NewBus(BusOptions{Persister: &fakePersister{}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		Follow(ctx, bus, Filter{}, FollowOptions{}, func(Event) {})
		close(done)
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Follow did not return after the context was canceled")
	}
}
