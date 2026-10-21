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
