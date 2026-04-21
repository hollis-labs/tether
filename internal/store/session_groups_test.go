package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/chrispian/agent-mux/internal/launch"
)

func TestSessionGroup_CreateGetList(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "sg.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	g := SessionGroupRow{
		ID:        "grp-1",
		Name:      "My Multiplexor",
		CreatedAt: "2026-04-21T10:00:00Z",
		UpdatedAt: "2026-04-21T10:00:00Z",
	}
	if err := db.CreateSessionGroup(g); err != nil {
		t.Fatalf("CreateSessionGroup: %v", err)
	}

	got, err := db.GetSessionGroup("grp-1")
	if err != nil {
		t.Fatalf("GetSessionGroup: %v", err)
	}
	if got.ID != "grp-1" || got.Name != "My Multiplexor" {
		t.Errorf("round-trip mismatch: %+v", got)
	}
}

func TestSessionGroup_GetMissing(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "sg2.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	_, err = db.GetSessionGroup("nope")
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("err = %v, want sql.ErrNoRows", err)
	}
}

func TestSessionGroup_AddMember(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "sg3.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	// Create group
	if err := db.CreateSessionGroup(SessionGroupRow{ID: "grp-A", Name: "A", CreatedAt: "2026-04-21T00:00:00Z", UpdatedAt: "2026-04-21T00:00:00Z"}); err != nil {
		t.Fatalf("create group: %v", err)
	}

	// Create a session (state=created is fine for membership test)
	row := SessionRow{ID: "s1", LaunchID: "l1", ProjectID: "p", LogicalAgentID: "a", ProviderID: "pv", Workspace: "/tmp", State: "created"}
	if err := db.CreateSession(row, &launch.Plan{LaunchID: "l1"}); err != nil {
		t.Fatalf("create session: %v", err)
	}

	if err := db.AddSessionToGroup("s1", "grp-A"); err != nil {
		t.Fatalf("AddSessionToGroup: %v", err)
	}

	// Verify via ListSessionsByGroup
	sessions, err := db.ListSessionsByGroup("grp-A")
	if err != nil {
		t.Fatalf("ListSessionsByGroup: %v", err)
	}
	if len(sessions) != 1 || sessions[0].ID != "s1" {
		t.Errorf("ListSessionsByGroup = %v, want [s1]", sessions)
	}
}

func TestSessionGroup_List(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "sg4.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	for _, id := range []string{"g1", "g2", "g3"} {
		if err := db.CreateSessionGroup(SessionGroupRow{ID: id, Name: id, CreatedAt: "2026-04-21T00:00:00Z", UpdatedAt: "2026-04-21T00:00:00Z"}); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}

	groups, err := db.ListSessionGroups()
	if err != nil {
		t.Fatalf("ListSessionGroups: %v", err)
	}
	if len(groups) != 3 {
		t.Errorf("got %d groups, want 3", len(groups))
	}
}
