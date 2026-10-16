package store

import (
	"context"
	"database/sql"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/launch"
)

func outboxRow(t *testing.T, db *Store, kind string) (eventID string, pending int, n int) {
	t.Helper()
	rows, err := db.db.Query(`SELECT COALESCE(event_id,''), pending FROM events WHERE kind = ?`, kind)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		n++
		if err := rows.Scan(&eventID, &pending); err != nil {
			t.Fatal(err)
		}
	}
	return eventID, pending, n
}

// The reaper's outcome event is an outbox row on the orphaning transaction: it
// commits with it, wakes the drainer, and is published once. When nothing is
// orphaned (a late natural completion wins) there is no event at all.
func TestReaperOrphanEventIsAnOutboxRowPublishedAfterCommit(t *testing.T) {
	db := openEventsStore(t)
	var kicks atomic.Int32
	bus := newOutboxBus(t, db)
	db.SetEventsCommittedHook(func() { kicks.Add(1); bus.KickOutbox() })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := bus.StartOutbox(ctx)
	defer stop()
	live, unsub, err := bus.(events.LiveSubscriber).SubscribeLive(ctx, events.Filter{Kinds: []string{"session.reaper"}})
	if err != nil {
		t.Fatal(err)
	}
	defer unsub()

	if err := db.CreateSession(SessionRow{ID: "done", State: "running"}, &launch.Plan{}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateSessionState("done", "completed", 0, new(int)); err != nil {
		t.Fatal(err)
	}
	if changed, err := db.RecordReaperOrphan(ctx, "done", "process_missing"); err != nil || changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if _, _, n := outboxRow(t, db, "session.reaper"); n != 0 || kicks.Load() != 0 {
		t.Fatalf("a no-op orphan left %d event(s), %d kick(s)", n, kicks.Load())
	}

	if err := db.CreateSession(SessionRow{ID: "lost", State: "running", PID: sql.NullInt64{Int64: 22, Valid: true}}, &launch.Plan{}); err != nil {
		t.Fatal(err)
	}
	if changed, err := db.RecordReaperOrphan(ctx, "lost", "process_missing"); err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	got := collect(t, live, 1, 5*time.Second)
	if got[0].SessionID != "lost" || !strings.HasPrefix(got[0].EventID, "evt_") || kicks.Load() != 1 {
		t.Fatalf("published %+v after %d kick(s)", got[0], kicks.Load())
	}
	expectNone(t, live, 300*time.Millisecond)
	if id, pending, n := outboxRow(t, db, "session.reaper"); n != 1 || pending != 0 || id != got[0].EventID {
		t.Fatalf("row id=%q pending=%d n=%d", id, pending, n)
	}
}

// Activity is written straight to the store by the reaper sweep; it now goes
// through the outbox so live subscribers see it.
func TestSessionActivityGoesThroughTheOutbox(t *testing.T) {
	db := openEventsStore(t)
	bus := newOutboxBus(t, db)
	db.SetEventsCommittedHook(bus.KickOutbox)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := bus.StartOutbox(ctx)
	defer stop()
	live, unsub, err := bus.(events.LiveSubscriber).SubscribeLive(ctx, events.Filter{Kinds: []string{"session.activity"}})
	if err != nil {
		t.Fatal(err)
	}
	defer unsub()
	if err := db.RecordSessionActivity(ctx, "s1", time.Now()); err != nil {
		t.Fatal(err)
	}
	got := collect(t, live, 1, 5*time.Second)
	if got[0].SessionID != "s1" || !strings.HasPrefix(got[0].EventID, "evt_") {
		t.Fatalf("published %+v", got[0])
	}
}

// The channel-attachment audit event (events.KindSessionTurnRouted) is written
// as an outbox row on the attachment's transaction, and a restarted process's
// drainer publishes what a crash left behind.
func TestChannelAttachmentAuditEventIsAnOutboxRow(t *testing.T) {
	f := newAccountingFixture(t, true, true) // settles through AttachChannelMessage
	id, pending, n := outboxRow(t, f.store, events.KindSessionTurnRouted)
	if n == 0 || !strings.HasPrefix(id, "evt_") || pending != 1 {
		t.Fatalf("attachment audit event: n=%d id=%q pending=%d", n, id, pending)
	}
	bus := newOutboxBus(t, f.store)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	live, unsub, err := bus.(events.LiveSubscriber).SubscribeLive(ctx, events.Filter{Kinds: []string{events.KindSessionTurnRouted}})
	if err != nil {
		t.Fatal(err)
	}
	defer unsub()
	stop := bus.StartOutbox(ctx)
	defer stop()
	collect(t, live, n, 5*time.Second)
	if _, pending, _ := outboxRow(t, f.store, events.KindSessionTurnRouted); pending != 0 {
		t.Fatal("still pending after the drain")
	}
}
