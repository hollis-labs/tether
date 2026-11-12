package app

import (
	"testing"

	"github.com/hollis-labs/libs/harness/core/adapters/agentsessions"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/provider/api/stub"
)

// A relaunch records itself as a continuation of the logical agent's previous
// session, taken from the daemon's rows (the old row is left in state created,
// as after a crash); every other launch stays fresh with no parent.
func TestRelaunchContinuity(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	svc := bindingHarness(t)
	svc.CatalogRoot = t.TempDir()
	svc.Catalog = &config.Catalog{
		Global:    config.Global{Version: "test"},
		Projects:  map[string]config.Project{"p": {ID: "p", RepoRoot: t.TempDir(), Workspace: config.WorkspaceSpec{SessionRoot: t.TempDir(), DefaultMode: "shared"}}},
		Agents:    map[string]config.Agent{"worker": {ID: "worker"}},
		Providers: map[string]config.Provider{"stub": {ID: "stub", Provider: "claude", RuntimeKind: config.RuntimeKindPTY, Command: "test-fake"}},
		Launches:  map[string]config.Launch{"l1": {ID: "l1", Project: "p", Agent: "worker", Provider: "stub"}},
	}
	svc.factories = map[string]RuntimeFactory{"stub": func(p *launch.Plan) (agentsessions.Runtime, error) { return stub.New(p) }}

	create := func(continuity string) (intent, parent string) {
		t.Helper()
		l, err := svc.CreateSessionWithInput(CreateSessionInput{LaunchID: "l1", Continuity: continuity})
		if err != nil {
			t.Fatal(err)
		}
		row, err := svc.Store.GetSession(l.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		return row.Intent, row.ParentSessionID.String
	}
	first := func() string {
		l, err := svc.CreateSessionWithInput(CreateSessionInput{LaunchID: "l1"})
		if err != nil {
			t.Fatal(err)
		}
		return l.SessionID
	}

	// No earlier session: a relaunch request is fresh.
	id1 := first()
	// (id1 now exists.) Default and explicit "fresh" stay fresh with no parent.
	if intent, parent := create(""); intent != "fresh" || parent != "" {
		t.Fatalf("default = %q/%q", intent, parent)
	}
	if intent, parent := create("fresh"); intent != "fresh" || parent != "" {
		t.Fatalf("fresh = %q/%q", intent, parent)
	}
	// Relaunch continues the newest earlier session of the agent.
	latest, err := svc.Store.LatestSessionForLogicalAgent("worker")
	if err != nil || latest == "" || latest == id1 {
		t.Fatalf("latest = %q err=%v", latest, err)
	}
	if intent, parent := create("relaunch"); intent != "resume" || parent != latest {
		t.Fatalf("relaunch = %q/%q, want resume/%q", intent, parent, latest)
	}
}

func TestContinuationWithoutAgentIsFresh(t *testing.T) {
	s := &Service{}
	if intent, parent := s.continuation(&launch.Plan{Continuity: "relaunch"}); intent != "" || parent.Valid {
		t.Fatalf("got %q/%+v", intent, parent)
	}
}
