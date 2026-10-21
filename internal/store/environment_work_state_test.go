package store

import (
	"context"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/events"
)

func workStateStore(t *testing.T, sessions ...string) *Store {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "ws.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, id := range sessions {
		if err := db.CreateSession(SessionRow{ID: id, LaunchID: "pm", ProjectID: "tether", ProviderID: "claude", ProviderKind: "cli", State: "running"}, nil); err != nil {
			t.Skipf("fixture needs a launch plan in this tree: %v", err)
		}
	}
	return db
}

func putWorkState(t *testing.T, db *Store, session, payload string) int64 {
	t.Helper()
	seq, _, err := db.InsertEvent(events.ScopeSession, session, events.KindSessionWorkStateChanged, payload)
	if err != nil {
		t.Fatal(err)
	}
	return seq
}

// CW-20261021-0003 (architect ruling A): the current work-state lives in
// environment_work_state, written in the event's transaction, and the snapshot
// reads that row.
func TestWorkStateRowFollowsTheEventAndSurvivesRetention(t *testing.T) {
	db := workStateStore(t, "a", "b", "none")
	ctx := context.Background()
	putWorkState(t, db, "a", `{"from":"unknown","to":"processing","at":"2026-10-21T00:00:01Z"}`)
	last := putWorkState(t, db, "a", `{"from":"processing","to":"idle","at":"2026-10-21T00:00:09Z"}`)
	putWorkState(t, db, "b", `{"from":"unknown","to":"processing","at":"2026-10-21T00:00:02Z"}`)

	w, ok, err := db.LatestWorkState(ctx, "a")
	if err != nil || !ok || w.Value != "idle" || w.At.Format("2006-01-02T15:04:05Z") != "2026-10-21T00:00:09Z" {
		t.Fatalf("a: %+v %v %v", w, ok, err)
	}
	if _, ok, err := db.LatestWorkState(ctx, "none"); err != nil || ok {
		t.Fatalf("a session that never reported must have no work-state (no backfill): %v %v", ok, err)
	}
	snap, err := db.EnvironmentSnapshot(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if snap.WorkStates["a"].Value != "idle" || snap.WorkStates["b"].Value != "processing" {
		t.Fatalf("snapshot: %+v", snap.WorkStates)
	}
	if _, ok := snap.WorkStates["none"]; ok {
		t.Fatal("a session that never reported got a work-state")
	}
	one, err := db.EnvironmentSnapshot(ctx, "a")
	if err != nil || len(one.WorkStates) != 1 || one.WorkStates["a"].Value != "idle" {
		t.Fatalf("per-session snapshot: %+v %v", one.WorkStates, err)
	}

	// Retention purges the events; the current value must not go with them.
	if _, err := db.db.Exec(`DELETE FROM events WHERE kind=?`, events.KindSessionWorkStateChanged); err != nil {
		t.Fatal(err)
	}
	snap, err = db.EnvironmentSnapshot(ctx, "")
	if err != nil || snap.WorkStates["a"].Value != "idle" {
		t.Fatalf("retention erased the current work-state: %+v %v", snap.WorkStates, err)
	}
	var seq int64
	if err := db.db.QueryRow(`SELECT event_seq FROM environment_work_state WHERE session_id='a'`).Scan(&seq); err != nil || seq != last {
		t.Fatalf("row event_seq %d, want %d (%v)", seq, last, err)
	}
}

// A stale or replayed event can never overwrite a newer value, and an event
// that names no known value is stored but leaves the row alone.
func TestWorkStateUpsertIgnoresStaleSequencesAndUnreadablePayloads(t *testing.T) {
	db := workStateStore(t, "a")
	seq := putWorkState(t, db, "a", `{"from":"unknown","to":"processing","at":"2026-10-21T00:00:05Z"}`)
	// The exact guard used by the transaction, with an older sequence.
	if _, err := db.db.Exec(upsertWorkStateSQL, "a", "idle", "2026-10-21T00:00:01Z", seq-1); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(upsertWorkStateSQL, "a", "idle", "2026-10-21T00:00:01Z", seq); err != nil {
		t.Fatal(err)
	}
	if w, _, _ := db.LatestWorkState(context.Background(), "a"); w.Value != "processing" {
		t.Fatalf("a stale or equal sequence overwrote the value: %+v", w)
	}
	putWorkState(t, db, "a", `{"to":"garbage"}`)
	putWorkState(t, db, "a", `not json`)
	if w, _, _ := db.LatestWorkState(context.Background(), "a"); w.Value != "processing" {
		t.Fatalf("an unreadable event changed the value: %+v", w)
	}
	var n int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM events WHERE session_id='a' AND kind=?`, events.KindSessionWorkStateChanged).Scan(&n); err != nil || n != 3 {
		t.Fatalf("the log must keep every event, got %d (%v)", n, err)
	}
	// A newer valid event does overwrite.
	putWorkState(t, db, "a", `{"from":"processing","to":"idle","at":"2026-10-21T00:00:09Z"}`)
	if w, _, _ := db.LatestWorkState(context.Background(), "a"); w.Value != "idle" {
		t.Fatalf("a newer event did not apply: %+v", w)
	}
}

// Atomicity: if the current-value row cannot be written, the event is not
// stored either, and the caller sees the error.
func TestWorkStateEventAndRowAreOneTransaction(t *testing.T) {
	db := workStateStore(t, "a")
	if _, err := db.db.Exec(`ALTER TABLE environment_work_state RENAME TO environment_work_state_off`); err != nil {
		t.Fatal(err)
	}
	var before int
	_ = db.db.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&before)
	_, _, err := db.InsertEvent(events.ScopeSession, "a", events.KindSessionWorkStateChanged, `{"from":"unknown","to":"idle","at":"2026-10-21T00:00:01Z"}`)
	if err == nil || !strings.Contains(err.Error(), "work-state") {
		t.Fatalf("a failed row write must fail the insert: %v", err)
	}
	var after int
	_ = db.db.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&after)
	if after != before {
		t.Fatalf("the event was stored without its row: %d -> %d events", before, after)
	}
	// The envelope path (the one the bus persister uses) is the same.
	if _, err := db.InsertEventEnvelope(context.Background(), events.Event{Scope: events.ScopeSession, SessionID: "a", Kind: events.KindSessionWorkStateChanged, PayloadJSON: `{"from":"unknown","to":"idle","at":"2026-10-21T00:00:01Z"}`}); err == nil {
		t.Fatal("the envelope path stored an event without its row")
	}
	_ = db.db.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&after)
	if after != before {
		t.Fatalf("the envelope path left an orphan event: %d -> %d", before, after)
	}
}

// The migration is additive: the table exists after Open, an existing database
// gains it without any session or event row changing, and a binary that does not
// know migration 0096 still opens a database that has it applied (the rollback
// case: the runner only applies the migrations it embeds and ignores recorded
// versions it does not know).
func TestMigration0096IsAdditiveAndRollbackSafe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mig.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM environment_work_state`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("table missing or not empty after migration: %d %v", n, err)
	}
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version=96`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("migration 96 not recorded: %d %v", n, err)
	}
	// Simulate the older binary: it knows no version above 95, so it must open a
	// database whose schema_migrations also lists 96 and a future 9999.
	if _, err := db.db.Exec(`INSERT INTO schema_migrations(version,name,applied_at) VALUES(9999,'from_the_future','2026-10-21T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := Open(path)
	if err != nil {
		t.Fatalf("a database with migrations this binary does not know no longer opens: %v", err)
	}
	defer again.Close()
	sub, err := fs.Sub(embeddedMigrations, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	res, err := migrateFS(again.db, sub)
	if err != nil || len(res.Applied) != 0 {
		t.Fatalf("re-running migrations changed something: %+v %v", res, err)
	}
}
