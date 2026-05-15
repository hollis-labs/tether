package store

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/tether/internal/launch"
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

// TestGetLaunchPlan_InjectionContentPersisted locks in the documented
// security-relevant behavior of CW-0115: injected file content (catalog and
// caller injection alike) IS persisted at rest in the launch_plans table.
// Plan native files and boot-dir overlay survive the JSON round-trip verbatim
// — which is exactly why injection content must be treated as non-secret.
func TestGetLaunchPlan_InjectionContentPersisted(t *testing.T) {
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
		NativeFiles: []launch.NativeFile{
			{Kind: "raw", RelPath: "NOTES.md", Content: "injected native file content"},
		},
		BootDirOverlay: map[string]string{
			"overlay.md": "injected overlay content",
		},
	}
	row := SessionRow{ID: "s1", LaunchID: "l1", ProjectID: "p", LogicalAgentID: "a", ProviderID: "pv", Workspace: "/tmp/ws", State: "created"}
	if err := db.CreateSession(row, want); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := db.GetLaunchPlan("s1")
	if err != nil {
		t.Fatalf("GetLaunchPlan: %v", err)
	}
	if len(got.NativeFiles) != 1 || got.NativeFiles[0].Content != "injected native file content" {
		t.Errorf("native file content not persisted at rest: %+v", got.NativeFiles)
	}
	if got.BootDirOverlay["overlay.md"] != "injected overlay content" {
		t.Errorf("boot-dir overlay content not persisted at rest: %+v", got.BootDirOverlay)
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

func TestSweepStaleSessions(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	plan := &launch.Plan{LaunchID: "l1"}

	type seedRow struct {
		id    string
		state string
		setTo string // final state after optional UpdateSessionState
	}
	seeds := []seedRow{
		{"s-created", "created", ""},
		{"s-launching", "created", "launching"},
		{"s-running", "created", "running"},
		{"s-completed", "created", "completed"},
		{"s-failed", "created", "failed"},
		{"s-killed", "created", "killed"},
	}
	exit := 0
	for _, s := range seeds {
		r := SessionRow{ID: s.id, LaunchID: "l1", ProjectID: "p", LogicalAgentID: "a", ProviderID: "pv", Workspace: "/tmp", State: "created"}
		if err := db.CreateSession(r, plan); err != nil {
			t.Fatalf("create %s: %v", s.id, err)
		}
		switch s.setTo {
		case "launching", "running":
			if err := db.UpdateSessionState(s.id, s.setTo, 0, nil); err != nil {
				t.Fatalf("update %s → %s: %v", s.id, s.setTo, err)
			}
		case "completed":
			if err := db.UpdateSessionState(s.id, s.setTo, 0, &exit); err != nil {
				t.Fatalf("update %s → %s: %v", s.id, s.setTo, err)
			}
		case "failed":
			code := 1
			if err := db.UpdateSessionState(s.id, s.setTo, 0, &code); err != nil {
				t.Fatalf("update %s → %s: %v", s.id, s.setTo, err)
			}
		case "killed":
			code := -1
			if err := db.UpdateSessionState(s.id, s.setTo, 0, &code); err != nil {
				t.Fatalf("update %s → %s: %v", s.id, s.setTo, err)
			}
		}
	}

	const sweepTime = "2026-04-21T12:00:00Z"
	n, err := db.SweepStaleSessions(sweepTime)
	if err != nil {
		t.Fatalf("SweepStaleSessions: %v", err)
	}
	if n != 2 {
		t.Errorf("swept %d sessions, want 2 (launching + running)", n)
	}

	// launching and running → failed with exit_code and ended_at set
	for _, id := range []string{"s-launching", "s-running"} {
		row, err := db.GetSession(id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if row.State != "failed" {
			t.Errorf("%s state = %q, want %q", id, row.State, "failed")
		}
		if !row.ExitCode.Valid {
			t.Errorf("%s exit_code not set after sweep", id)
		}
		if !row.EndedAt.Valid {
			t.Errorf("%s ended_at not set after sweep", id)
		}
		if row.EndedAt.String != sweepTime {
			t.Errorf("%s ended_at = %q, want %q", id, row.EndedAt.String, sweepTime)
		}
	}

	// all other states untouched
	for _, tc := range []struct{ id, want string }{
		{"s-created", "created"},
		{"s-completed", "completed"},
		{"s-failed", "failed"},
		{"s-killed", "killed"},
	} {
		row, err := db.GetSession(tc.id)
		if err != nil {
			t.Fatalf("get %s: %v", tc.id, err)
		}
		if row.State != tc.want {
			t.Errorf("%s state = %q, want %q (should be untouched)", tc.id, row.State, tc.want)
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
