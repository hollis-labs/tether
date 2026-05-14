package events_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/store"
)

// The bus must accept *store.Store as a Persister. This test exists
// as much to compile-fail if the two interfaces drift as it does to
// exercise round-trip behavior.
func TestBus_StoreBacked_RoundTrip(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "ints.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	bus := events.NewBus(events.BusOptions{Persister: db})
	ctx := context.Background()

	ch, cancel, err := bus.Subscribe(ctx, events.Filter{})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cancel()

	if err := bus.Publish(ctx, events.Event{
		Scope:       events.ScopeSession,
		SessionID:   "s1",
		Kind:        "session.started",
		PayloadJSON: `{"to":"running"}`,
	}); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	select {
	case got := <-ch:
		if got.Seq != 1 {
			t.Errorf("Seq = %d, want 1", got.Seq)
		}
		if got.Scope != events.ScopeSession {
			t.Errorf("Scope = %q", got.Scope)
		}
		if got.Kind != "session.started" {
			t.Errorf("Kind = %q", got.Kind)
		}
		if got.PayloadJSON != `{"to":"running"}` {
			t.Errorf("PayloadJSON = %q", got.PayloadJSON)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout")
	}

	// Publish a second event and verify SinceSeq=1 replay produces
	// only it — the full round-trip through SQLite.
	if err := bus.Publish(ctx, events.Event{
		Scope: events.ScopeDaemon,
		Kind:  "daemon.started",
	}); err != nil {
		t.Fatal(err)
	}
	// Drain the live channel so the next subscription starts clean.
	<-ch

	ch2, cancel2, err := bus.Subscribe(ctx, events.Filter{SinceSeq: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer cancel2()

	select {
	case got := <-ch2:
		if got.Seq != 2 || got.Scope != events.ScopeDaemon {
			t.Errorf("replay got seq=%d scope=%q, want seq=2 scope=daemon", got.Seq, got.Scope)
		}
	case <-time.After(time.Second):
		t.Fatal("replay timeout")
	}
}
