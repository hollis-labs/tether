package store

import (
	"database/sql"
	"github.com/hollis-labs/tether/internal/launch"
	"testing"
)

func appRow(id, parent, app string) SessionRow {
	r := SessionRow{ID: id, LaunchID: "l", ProjectID: "p", ProviderID: "prov", ProviderKind: "cli", Workspace: "/w", State: "created"}
	if parent != "" {
		r.ParentSessionID = sql.NullString{String: parent, Valid: true}
	}
	if app != "" {
		r.App = sql.NullString{String: app, Valid: true}
	}
	return r
}

func sessionApp(t *testing.T, s *Store, id string) (string, bool) {
	t.Helper()
	row, err := s.GetSession(id)
	if err != nil {
		t.Fatal(err)
	}
	return row.App.String, row.App.Valid
}

func TestSessionAppIsStoredAtCreationAndNullByDefault(t *testing.T) {
	s := openWorkstreamStore(t)
	if err := s.CreateSession(appRow("a1", "", "torque"), &launch.Plan{}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession(appRow("a2", "", ""), &launch.Plan{}); err != nil {
		t.Fatal(err)
	}
	if app, ok := sessionApp(t, s, "a1"); !ok || app != "torque" {
		t.Fatalf("stamped app = %q valid=%v", app, ok)
	}
	if _, ok := sessionApp(t, s, "a2"); ok {
		t.Fatal("a session with no stamp has an app")
	}
	rows, err := s.ListSessions(ListSessionsOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	for _, r := range rows {
		seen[r.ID] = r.App.String
	}
	if seen["a1"] != "torque" || seen["a2"] != "" {
		t.Fatalf("list lost the app: %v", seen)
	}
}

func TestChildSessionTakesExactlyItsParentsApp(t *testing.T) {
	s := openWorkstreamStore(t)
	for _, r := range []SessionRow{appRow("p-torque", "", "torque"), appRow("p-none", "", "")} {
		if err := s.CreateSession(r, &launch.Plan{}); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		name, id, parent, carries, want string
	}{
		{"inherits the parent's app", "c1", "p-torque", "", "torque"},
		{"cannot widen to another app", "c2", "p-torque", "cerberus", "torque"},
		{"parent with no app gives none even if the row names one", "c3", "p-none", "torque", ""},
		{"an unreadable parent gives none", "c4", "ghost", "torque", ""},
	}
	for _, tc := range cases {
		if err := s.CreateSession(appRow(tc.id, tc.parent, tc.carries), &launch.Plan{}); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got, _ := sessionApp(t, s, tc.id); got != tc.want {
			t.Errorf("%s: app = %q, want %q", tc.name, got, tc.want)
		}
	}
	// Grandchildren keep the line: inheritance is transitive through each creation.
	if err := s.CreateSession(appRow("g1", "c1", "cerberus"), &launch.Plan{}); err != nil {
		t.Fatal(err)
	}
	if got, _ := sessionApp(t, s, "g1"); got != "torque" {
		t.Errorf("grandchild app = %q", got)
	}
}

func TestSessionAppHasNoUpdatePath(t *testing.T) {
	s := openWorkstreamStore(t)
	if err := s.CreateSession(appRow("u1", "", "torque"), &launch.Plan{}); err != nil {
		t.Fatal(err)
	}
	exit := 0
	if err := s.UpdateSessionState("u1", "completed", 0, &exit); err != nil {
		t.Fatal(err)
	}
	if got, _ := sessionApp(t, s, "u1"); got != "torque" {
		t.Fatalf("app changed by a state update: %q", got)
	}
}
