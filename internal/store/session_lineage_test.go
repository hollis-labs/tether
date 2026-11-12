package store

import (
	"database/sql"
	"reflect"
	"testing"

	"github.com/hollis-labs/tether/internal/launch"
)

func TestSessionLineage_ChildrenAndDepth(t *testing.T) {
	db := openTestStoreForSessions(t)
	mk := func(id, parent string) {
		t.Helper()
		row := SessionRow{ID: id, LaunchID: "l", ProjectID: "p", LogicalAgentID: "a", ProviderID: "pv", Workspace: "/tmp/ws", State: "created",
			ParentSessionID: sql.NullString{String: parent, Valid: parent != ""}}
		if err := db.CreateSession(row, &launch.Plan{LaunchID: "l"}); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	mk("root", "")
	mk("a", "root")
	mk("b", "root")
	mk("c", "a")
	kids, err := db.ChildSessionIDs("root")
	if err != nil || !reflect.DeepEqual(kids, []string{"a", "b"}) {
		t.Fatalf("children = %v, %v", kids, err)
	}
	for id, want := range map[string]int{"root": 0, "a": 1, "c": 2} {
		if got, err := db.SessionLineageDepth(id); err != nil || got != want {
			t.Fatalf("depth(%s) = %d, %v; want %d", id, got, err, want)
		}
	}
}

// created_at has one-second resolution: rows inserted in the same second order
// by insertion, and a still-running row of another launch still counts (any state).
func TestLatestSessionForLogicalAgent_TieAndStaleRunning(t *testing.T) {
	db := openTestStoreForSessions(t)
	mk := func(id, launchID, state string) {
		t.Helper()
		row := SessionRow{ID: id, LaunchID: launchID, ProjectID: "p", LogicalAgentID: "ag", ProviderID: "pv", Workspace: "/tmp/ws", State: state}
		if err := db.CreateSession(row, &launch.Plan{LaunchID: launchID}); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	if got, err := db.LatestSessionForLogicalAgent("ag"); err != nil || got != "" {
		t.Fatalf("empty = %q, %v", got, err)
	}
	// Ids sort opposite to insertion order, so an id tie-break would pick "a".
	mk("z", "l1", "running")
	mk("m", "l2", "created")
	mk("a", "l3", "running")
	if _, err := db.db.Exec(`UPDATE sessions SET created_at='2026-01-01T00:00:00Z'`); err != nil {
		t.Fatal(err)
	}
	if got, err := db.LatestSessionForLogicalAgent("ag"); err != nil || got != "a" {
		t.Fatalf("same-second tie = %q, %v; want a (last inserted, still running)", got, err)
	}
	if _, err := db.db.Exec(`UPDATE sessions SET created_at='2026-01-01T00:00:05Z' WHERE id='z'`); err != nil {
		t.Fatal(err)
	}
	if got, err := db.LatestSessionForLogicalAgent("ag"); err != nil || got != "z" {
		t.Fatalf("newer running row = %q, %v; want z", got, err)
	}
}
