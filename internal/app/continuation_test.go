package app

import (
	"github.com/hollis-labs/tether/internal/api"
	"strings"
	"sync"
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
	svc := relaunchHarness(t)
	runRelaunchContinuity(t, svc)
}

func relaunchHarness(t *testing.T) *Service {
	t.Helper()
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
	return svc
}

func runRelaunchContinuity(t *testing.T, svc *Service) {
	t.Helper()
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

// Concurrent relaunches of one agent each see the other's row and form a chain;
// none shares a parent, and every parent is an earlier session.
func TestConcurrentRelaunchesDoNotShareParent(t *testing.T) {
	svc := relaunchHarness(t)
	first, err := svc.CreateSessionWithInput(CreateSessionInput{LaunchID: "l1"})
	if err != nil {
		t.Fatal(err)
	}
	const n = 8
	ids := make([]string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			l, err := svc.CreateSessionWithInput(CreateSessionInput{LaunchID: "l1", Continuity: "relaunch"})
			if err != nil {
				t.Error(err)
				return
			}
			ids[i] = l.SessionID
		}(i)
	}
	wg.Wait()
	parents := map[string]bool{}
	for _, id := range ids {
		if id == "" {
			t.Fatal("a relaunch failed")
		}
		row, err := svc.Store.GetSession(id)
		if err != nil {
			t.Fatal(err)
		}
		p := row.ParentSessionID.String
		if row.Intent != "resume" || p == "" || p == id {
			t.Fatalf("session %s intent=%q parent=%q", id, row.Intent, p)
		}
		if parents[p] {
			t.Fatalf("parent %s chosen by more than one relaunch", p)
		}
		parents[p] = true
	}
	if !parents[first.SessionID] {
		t.Fatalf("no relaunch continued the first session; parents=%v", parents)
	}
}

// A logical agent whose launch_id cache was never written still resumes from
// the launch recorded on its newest session row.
func TestResumeFallsBackToSessionLaunchID(t *testing.T) {
	svc := relaunchHarness(t)
	l, err := svc.CreateSessionWithInput(CreateSessionInput{LaunchID: "l1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Store.DB().Exec(`UPDATE logical_agents SET launch_id=NULL WHERE id='worker'`); err != nil {
		t.Fatal(err)
	}
	_ = l
	_, err = svc.ResumeLogicalAgent("worker", api.ResumeOptions{})
	if err != nil && strings.Contains(err.Error(), "never launched") {
		t.Fatalf("resume ignored the session's launch_id: %v", err)
	}
}
