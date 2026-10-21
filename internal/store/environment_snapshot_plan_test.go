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

func explainPlan(t *testing.T, db *Store, query string, args ...any) string {
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

// CW-20261021-0003: the latest work-state of a session is an index seek on
// (session_id, kind, at), never a sort of the session's events.
func TestWorkStateLookupsUseTheSessionKindIndex(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "ws-plan.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for name, plan := range map[string]string{
		"latest":   explainPlan(t, db, latestWorkStateSQL, "s", events.KindSessionWorkStateChanged),
		"snapshot": explainPlan(t, db, environmentWorkStatesSQL, events.KindSessionWorkStateChanged, "", ""),
	} {
		if strings.Contains(plan, "TEMP B-TREE") || !strings.Contains(plan, "idx_events_session_kind_at") {
			t.Fatalf("%s: work-state lookup does not use the session/kind/time index without a sort: %s", name, plan)
		}
	}
}

func TestLatestWorkStateAndSnapshot(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "ws.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	for _, id := range []string{"a", "b", "none"} {
		if err := db.CreateSession(SessionRow{ID: id, LaunchID: "pm", ProjectID: "tether", ProviderID: "claude", ProviderKind: "cli", State: "running"}, nil); err != nil {
			t.Skipf("fixture needs a launch plan in this tree: %v", err)
		}
	}
	put := func(session, payload string) {
		t.Helper()
		if _, _, err := db.InsertEvent(events.ScopeSession, session, events.KindSessionWorkStateChanged, payload); err != nil {
			t.Fatal(err)
		}
	}
	put("a", `{"from":"unknown","to":"processing","at":"2026-10-21T00:00:01Z"}`)
	put("a", `{"from":"processing","to":"idle","at":"2026-10-21T00:00:09Z"}`)
	put("b", `{"from":"unknown","to":"processing","at":"2026-10-21T00:00:02Z"}`)
	put("b", `{"to":"garbage"}`) // an unreadable newest payload is no work-state, never a stale older one

	if w, ok, err := db.LatestWorkState(ctx, "a"); err != nil || !ok || w.Value != "idle" || w.At.IsZero() {
		t.Fatalf("a: %+v %v %v", w, ok, err)
	}
	if _, ok, err := db.LatestWorkState(ctx, "none"); err != nil || ok {
		t.Fatalf("none: %v %v", ok, err)
	}
	if _, ok, _ := db.LatestWorkState(ctx, "b"); ok {
		t.Fatal("an unreadable newest payload fell back to an older value")
	}
	snap, err := db.EnvironmentSnapshot(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := snap.WorkStates["a"]; got.Value != "idle" {
		t.Fatalf("snapshot a: %+v", snap.WorkStates)
	}
	if _, ok := snap.WorkStates["none"]; ok {
		t.Fatal("a session with no work-state event got one")
	}
	one, err := db.EnvironmentSnapshot(ctx, "a")
	if err != nil || len(one.WorkStates) != 1 || one.WorkStates["a"].Value != "idle" {
		t.Fatalf("per-session snapshot: %+v %v", one.WorkStates, err)
	}
}
