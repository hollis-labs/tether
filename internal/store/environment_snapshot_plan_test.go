package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/events"
)

// CW-20261021-0002: the snapshot's per-session last-event time must stay an
// index lookup. The earlier ORDER BY id DESC form made SQLite read and sort
// every event of every session on each call (10 s on the first call against the
// production database). The plan is asserted, not a timing, so it cannot flake.
func TestEnvironmentSnapshotLastEventUsesTheSessionIndex(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "plan.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.db.Query(`EXPLAIN QUERY PLAN `+environmentSessionsSQL, "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	all := strings.Join(plan, " | ")
	if strings.Contains(all, "TEMP B-TREE") || !strings.Contains(all, "idx_events_session") {
		t.Fatalf("last-event lookup no longer uses the session index without a sort: %s", all)
	}
}

// The snapshot still reports the newest event time per session, and a session
// with no events reports none.
func TestEnvironmentSnapshotLastEventAt(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "last.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, id := range []string{"quiet", "busy"} {
		if err := db.CreateSession(SessionRow{ID: id, LaunchID: "pm", ProjectID: "tether", ProviderID: "claude", ProviderKind: "cli", State: "running"}, nil); err != nil {
			t.Skipf("fixture needs a launch plan in this tree: %v", err)
		}
	}
	for _, kind := range []string{"session.output", "session.state_changed"} {
		if _, _, err := db.InsertEvent(events.ScopeSession, "busy", kind, `{}`); err != nil {
			t.Fatal(err)
		}
	}
	snap, err := db.EnvironmentSnapshot(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range snap.Sessions {
		got[r.ID] = r.LastEventAt
	}
	if got["busy"] == "" || got["quiet"] != "" {
		t.Fatalf("last event times: %v", got)
	}
}

func explain(t *testing.T, db *Store, query string, args ...any) string {
	t.Helper()
	rows, err := db.db.Query(`EXPLAIN QUERY PLAN `+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	return strings.Join(plan, " | ")
}

// CW-20261024-0055: the event-id bounds are read on every environment snapshot,
// events read and bus subscription, so they must not scan the events table. SQLite
// only answers MIN/MAX from the b-tree edge when each is alone in its SELECT; the
// combined form scanned 1.8M rows (92 ms warm, seconds cold) on the hub. The plan is
// asserted, not a timing, so it cannot flake.
func TestEventBoundsQueriesDoNotScanEvents(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "bounds-plan.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for name, q := range map[string]string{"environment": environmentBoundsSQL, "stream": streamBoundsSQL} {
		plan := explain(t, db, q)
		for _, step := range strings.Split(plan, " | ") {
			if strings.HasPrefix(strings.TrimSpace(strings.TrimLeft(step, "|-` ")), "SCAN events") {
				t.Errorf("%s bounds query scans the events table: %s", name, plan)
			}
		}
		if !strings.Contains(plan, "SEARCH events") {
			t.Errorf("%s bounds query should search the events b-tree edges: %s", name, plan)
		}
	}
}

// The rewrite returns the same bounds as before, including the cases the
// sqlite_sequence term exists for: an empty table, and ids purged from the top.
func TestEventBoundsValues(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "bounds-values.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	read := func() (head, earliest int64) {
		t.Helper()
		tx, err := db.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback() }()
		head, earliest, err = environmentBounds(ctx, tx)
		if err != nil {
			t.Fatal(err)
		}
		return head, earliest
	}
	if head, earliest := read(); head != 0 || earliest != 1 {
		t.Fatalf("empty table: head=%d earliest=%d, want 0 and head+1", head, earliest)
	}
	for i := 0; i < 5; i++ {
		if _, _, err := db.InsertEvent(events.ScopeSession, "s", "session.output", `{}`); err != nil {
			t.Fatal(err)
		}
	}
	if head, earliest := read(); head != 5 || earliest != 1 {
		t.Fatalf("five events: head=%d earliest=%d", head, earliest)
	}
	// Purge the newest two and the oldest one: the cursor must not rewind to the
	// highest surviving id, and the earliest available moves up.
	if _, err := db.db.Exec(`DELETE FROM events WHERE id IN (1, 4, 5)`); err != nil {
		t.Fatal(err)
	}
	if head, earliest := read(); head != 5 || earliest != 2 {
		t.Fatalf("after purge: head=%d (sqlite_sequence keeps 5) earliest=%d (want 2)", head, earliest)
	}
	lo, hi, err := db.StreamBoundsContext(ctx)
	if err != nil || lo != 2 || hi != 3 {
		t.Fatalf("stream bounds = %d..%d err=%v, want 2..3", lo, hi, err)
	}
}
