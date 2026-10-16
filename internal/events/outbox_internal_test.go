package events

import (
	"context"
	"sync"
	"testing"
	"time"
)

// fakeTxPersister is an in-memory outbox: PublishTx appends a pending row; the
// pending query can run a hook right after it looks (to land a row "between the
// query and the dirty flag").
type fakeTxPersister struct {
	fakePersister
	pmu        sync.Mutex
	pending    []Event
	afterQuery func()
	failQuery  bool
}

func (f *fakeTxPersister) InsertEventTx(_ context.Context, _ Tx, e Event) (Event, error) {
	f.pmu.Lock()
	defer f.pmu.Unlock()
	f.mu.Lock()
	f.seq++
	e.Seq = f.seq
	f.mu.Unlock()
	e.At = time.Now().UTC()
	e.EventID = "evt_fake"
	f.pending = append(f.pending, e)
	return e, nil
}

func (f *fakeTxPersister) PendingEventsContext(_ context.Context, before int64, limit int) ([]Event, error) {
	f.pmu.Lock()
	if f.failQuery {
		f.pmu.Unlock()
		return nil, context.DeadlineExceeded
	}
	var out []Event
	for _, e := range f.pending {
		if (before <= 0 || e.Seq < before) && len(out) < limit {
			out = append(out, e)
		}
	}
	hook := f.afterQuery
	f.afterQuery = nil
	f.pmu.Unlock()
	if hook != nil {
		hook()
	}
	return out, nil
}

func (f *fakeTxPersister) MarkPublishedContext(_ context.Context, seqs []int64) error {
	f.pmu.Lock()
	defer f.pmu.Unlock()
	keep := f.pending[:0]
	for _, e := range f.pending {
		published := false
		for _, s := range seqs {
			if s == e.Seq {
				published = true
			}
		}
		if !published {
			keep = append(keep, e)
		}
	}
	f.pending = keep
	return nil
}

func (f *fakeTxPersister) pendingCount() int {
	f.pmu.Lock()
	defer f.pmu.Unlock()
	return len(f.pending)
}

// A PublishTx that lands after the drain looked but before it finished must
// leave the outbox dirty: the flag is cleared before the look, not after.
func TestOutboxDirtyFlagIsClearedBeforeTheDrainLooks(t *testing.T) {
	ft := &fakeTxPersister{}
	bus := NewBus(BusOptions{Persister: ft, OutboxSweep: time.Hour})
	b := bus.(*memBus)
	ctx := context.Background()
	if _, err := b.PublishTx(ctx, nil, Event{Scope: ScopeSession, Kind: "first"}); err != nil {
		t.Fatal(err)
	}
	ft.afterQuery = func() { // lands between the drain's query and the end of the drain
		if _, err := b.PublishTx(ctx, nil, Event{Scope: ScopeSession, Kind: "second"}); err != nil {
			t.Error(err)
		}
	}
	b.drainOnce(ctx)
	if ft.pendingCount() != 1 {
		t.Fatalf("pending after the first drain = %d, want the late row only", ft.pendingCount())
	}
	if !b.dirty.Load() {
		t.Fatal("a row committed after the drain looked left the outbox marked clean: it would wait for the next sweep")
	}
	b.drainOnce(ctx)
	if ft.pendingCount() != 0 || b.dirty.Load() {
		t.Fatalf("second drain left pending=%d dirty=%v", ft.pendingCount(), b.dirty.Load())
	}
}

// A bus over a TxPersister starts dirty: rows a crashed process left pending
// are drained by the first Publish even if StartOutbox has not run yet.
func TestOutboxStartsDirtyAndAFailedPreDrainStopsPublish(t *testing.T) {
	ft := &fakeTxPersister{}
	if _, err := ft.InsertEventTx(context.Background(), nil, Event{Scope: ScopeSession, Kind: "left-behind"}); err != nil {
		t.Fatal(err)
	}
	bus := NewBus(BusOptions{Persister: ft, OutboxSweep: time.Hour})
	if !bus.(*memBus).dirty.Load() {
		t.Fatal("a bus over a TxPersister must start dirty")
	}
	live, unsub, err := bus.(LiveSubscriber).SubscribeLive(context.Background(), Filter{})
	if err != nil {
		t.Fatal(err)
	}
	defer unsub()
	// the drain cannot read the outbox: nothing is persisted, nothing fans out
	ft.failQuery = true
	before := len(ft.events)
	if err := bus.Publish(context.Background(), Event{Scope: ScopeDaemon, Kind: "normal"}); err == nil {
		t.Fatal("Publish went ahead over an outbox it could not read")
	}
	if len(ft.events) != before {
		t.Fatal("a failed pre-drain still inserted the event")
	}
	ft.failQuery = false
	if err := bus.Publish(context.Background(), Event{Scope: ScopeDaemon, Kind: "normal"}); err != nil {
		t.Fatal(err)
	}
	var got []string
	for len(got) < 2 {
		select {
		case e := <-live:
			if e.Kind != KindStreamGap {
				got = append(got, e.Kind)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("got %v", got)
		}
	}
	if got[0] != "left-behind" || got[1] != "normal" {
		t.Fatalf("order %v", got)
	}
}
