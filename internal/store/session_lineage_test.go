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
