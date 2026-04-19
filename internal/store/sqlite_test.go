package store

import (
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
	rows, err := db.ListSessions()
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
