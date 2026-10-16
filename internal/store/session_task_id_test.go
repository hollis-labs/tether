package store

import (
	"database/sql"
	"testing"
)

// The launch-time task stamp survives every read path and is NULL when none was named.
func TestSessionTaskIDRoundTrip(t *testing.T) {
	s := openTestStoreForSessions(t)
	with := SessionRow{ID: "with", LaunchID: "l", ProjectID: "p", ProviderID: "x", ProviderKind: "cli", Workspace: "/w", State: "created", TaskID: sql.NullString{String: "CW-20261016-0155", Valid: true}}
	without := SessionRow{ID: "without", LaunchID: "l", ProjectID: "p", ProviderID: "x", ProviderKind: "cli", Workspace: "/w", State: "created"}
	for _, r := range []SessionRow{with, without} {
		if err := s.CreateSession(r, nil); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.GetSession("with")
	if err != nil || !got.TaskID.Valid || got.TaskID.String != "CW-20261016-0155" {
		t.Fatalf("GetSession task = %+v err=%v", got.TaskID, err)
	}
	if got, err = s.GetSession("without"); err != nil || got.TaskID.Valid {
		t.Fatalf("unstamped session task = %+v err=%v, want NULL", got.TaskID, err)
	}
	rows, err := s.ListSessions(ListSessionsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	for _, r := range rows {
		seen[r.ID] = r.TaskID.String
	}
	if seen["with"] != "CW-20261016-0155" || seen["without"] != "" {
		t.Fatalf("ListSessions tasks = %v", seen)
	}
}
