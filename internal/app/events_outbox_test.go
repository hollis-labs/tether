package app

import (
	"context"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/events"
)

// StartEventsOutbox publishes what an earlier process committed but never
// published, publishes later rows after their commit hook, is idempotent to
// start twice, and stops cleanly.
func TestServiceEventsOutboxPublishesLeftoversAndNewRows(t *testing.T) {
	svc, _, _, _ := credentialLaunch(t)
	svc.Bus = events.NewBus(events.BusOptions{Persister: svc.Store, OutboxSweep: time.Hour})
	svc.Store.SetEventsCommittedHook(svc.Bus.(events.OutboxBus).KickOutbox)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// a row committed by a previous process that died before publishing it
	tx, err := svc.Store.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	left, err := svc.Bus.(events.OutboxBus).PublishTx(ctx, tx, events.Event{Scope: events.ScopeSession, SessionID: "s", Kind: "session.reaper", PayloadJSON: `{}`})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	live, unsub, err := svc.Bus.(events.LiveSubscriber).SubscribeLive(ctx, events.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	defer unsub()
	svc.StartEventsOutbox(ctx)
	svc.StartEventsOutbox(ctx) // idempotent
	got := waitEvent(t, live)
	if got.EventID != left.EventID || got.Seq != left.Seq {
		t.Fatalf("leftover published as %+v, want %+v", got, left)
	}

	// a new outbox row is published after the store reports the commit
	if err := svc.Store.RecordSessionActivity(ctx, "s", time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := waitEvent(t, live); got.Kind != "session.activity" || got.EventID == "" {
		t.Fatalf("new row published as %+v", got)
	}
	svc.stopEventsOutbox()
	svc.stopEventsOutbox() // stopping twice is fine
}

func waitEvent(t *testing.T, ch <-chan events.Event) events.Event {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case e := <-ch:
			if e.Kind == events.KindStreamGap {
				continue
			}
			return e
		case <-deadline:
			t.Fatal("no event published")
		}
	}
}
