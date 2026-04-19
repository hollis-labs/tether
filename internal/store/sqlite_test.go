package store

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/chrispian/agent-mux/internal/launch"
)

func TestCreateAndListSession(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	row := SessionRow{ID: "s1", LaunchID: "l1", ProjectID: "p", LogicalAgentID: "a", ProviderID: "pv", Workspace: "/tmp/ws", State: "created"}
	if err := db.CreateSession(row, &launch.Plan{LaunchID: "l1"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	rows, err := db.ListSessions(ListSessionsOptions{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != "s1" {
		t.Fatalf("unexpected rows: %+v", rows)
	}
	exit := 0
	if err := db.UpdateSessionState("s1", "completed", 1234, &exit); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, err := db.GetSession("s1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.State != "completed" || !got.ExitCode.Valid || got.ExitCode.Int64 != 0 {
		t.Fatalf("state not updated: %+v", got)
	}
}

func TestGetLaunchPlan_RoundTrip(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	want := &launch.Plan{
		LaunchID:       "l1",
		ProjectID:      "p",
		LogicalAgentID: "a",
		ProviderID:     "pv",
		Command:        "echo",
		Args:           []string{"hi"},
	}
	row := SessionRow{ID: "s1", LaunchID: "l1", ProjectID: "p", LogicalAgentID: "a", ProviderID: "pv", Workspace: "/tmp/ws", State: "created"}
	if err := db.CreateSession(row, want); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := db.GetLaunchPlan("s1")
	if err != nil {
		t.Fatalf("GetLaunchPlan: %v", err)
	}
	if got.LaunchID != want.LaunchID || got.Command != want.Command || len(got.Args) != 1 || got.Args[0] != "hi" {
		t.Errorf("roundtrip mismatch: got %+v want %+v", got, want)
	}
}

func TestListSessions_LimitCursorState(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	// Seed 5 sessions with sortable created_at values; alternate two states.
	// We bypass CreateSession's auto-timestamp by writing directly — it's
	// the only way to control ORDER BY semantics in a test.
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("s%d", i)
		state := "running"
		if i%2 == 0 {
			state = "completed"
		}
		createdAt := fmt.Sprintf("2026-04-19T10:0%d:00Z", i)
		if _, err := db.db.Exec(
			`INSERT INTO sessions (id, launch_id, project_id, logical_agent_id, provider_id, workspace, state, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, "l", "p", "a", "pv", "/tmp/ws", state, createdAt, createdAt,
		); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}

	// Limit: 2 → should get the two newest.
	rows, err := db.ListSessions(ListSessionsOptions{Limit: 2})
	if err != nil {
		t.Fatalf("ListSessions limit: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("limit=2 returned %d rows", len(rows))
	}
	if rows[0].ID != "s4" || rows[1].ID != "s3" {
		t.Errorf("limit=2 order: got %s,%s want s4,s3", rows[0].ID, rows[1].ID)
	}

	// Cursor: should skip strict-newer rows.
	rows, err = db.ListSessions(ListSessionsOptions{Cursor: "2026-04-19T10:03:00Z"})
	if err != nil {
		t.Fatalf("ListSessions cursor: %v", err)
	}
	if len(rows) != 3 || rows[0].ID != "s2" {
		t.Errorf("cursor: got %d rows, first=%s", len(rows), rows[0].ID)
	}

	// State filter: only running rows (s1, s3).
	rows, err = db.ListSessions(ListSessionsOptions{State: "running"})
	if err != nil {
		t.Fatalf("ListSessions state: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("state=running returned %d rows", len(rows))
	}
	for _, r := range rows {
		if r.State != "running" {
			t.Errorf("state filter leaked row %s with state=%s", r.ID, r.State)
		}
	}
}

func TestGetLaunchPlan_Missing(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	_, err = db.GetLaunchPlan("missing")
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("err = %v, want sql.ErrNoRows", err)
	}
}
