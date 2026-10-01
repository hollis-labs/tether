package app

// CW-20261001-0130: an agent naming a sandbox profile the catalog does not
// define is refused, at create and at launch, instead of running with no
// sandbox. An empty name keeps meaning "no sandbox".

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/agentkit/agentsessions"
	"github.com/hollis-labs/go-sandbox/sandbox"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/provider/api/stub"
	"github.com/hollis-labs/tether/internal/session"
	"github.com/hollis-labs/tether/internal/store"
)

func sandboxProfiles() map[string]sandbox.Profile {
	return map[string]sandbox.Profile{"workspace-only": {ID: "workspace-only"}}
}

func TestApplyAgentOps_UnknownSandboxProfileRefused(t *testing.T) {
	svc := buildTestService(t, map[string]config.Agent{
		"test-agent": {ID: "test-agent", Permissions: config.AgentPermissions{DefaultSandbox: "no-such-profile"}},
	}, t.TempDir())
	svc.Catalog.SandboxProfiles = sandboxProfiles()

	err := svc.applyAgentOps(basePlan(), CreateSessionInput{LaunchID: "test-launch"})
	if !errors.Is(err, config.ErrUnknownSandboxProfile) {
		t.Fatalf("err = %v; want ErrUnknownSandboxProfile", err)
	}
	if !strings.Contains(err.Error(), `"no-such-profile"`) || !strings.Contains(err.Error(), `"test-agent"`) {
		t.Fatalf("error %q does not name the profile and the agent", err)
	}
}

// An agent_inline override naming an undefined profile is refused too.
func TestApplyAgentOps_InlineOverrideUnknownSandboxRefused(t *testing.T) {
	svc := buildTestService(t, map[string]config.Agent{
		"test-agent": {ID: "test-agent"},
	}, t.TempDir())
	svc.Catalog.SandboxProfiles = sandboxProfiles()

	in := CreateSessionInput{LaunchID: "test-launch", AgentInline: `{"permissions":{"default_sandbox":"typo-profile"}}`}
	if err := svc.applyAgentOps(basePlan(), in); !errors.Is(err, config.ErrUnknownSandboxProfile) {
		t.Fatalf("err = %v; want ErrUnknownSandboxProfile", err)
	}
}

func TestApplyAgentOps_KnownOrEmptySandboxAccepted(t *testing.T) {
	for _, name := range []string{"workspace-only", ""} {
		svc := buildTestService(t, map[string]config.Agent{
			"test-agent": {ID: "test-agent", Permissions: config.AgentPermissions{DefaultSandbox: name}},
		}, t.TempDir())
		svc.Catalog.SandboxProfiles = sandboxProfiles()
		if err := svc.applyAgentOps(basePlan(), CreateSessionInput{LaunchID: "test-launch"}); err != nil {
			t.Fatalf("default_sandbox %q: %v", name, err)
		}
	}
}

// profileCapture records the sandbox profile LaunchSession hands the runtime.
type profileCapture struct {
	agentsessions.Runtime
	started *bool
	profile *sandbox.Profile
}

func (p profileCapture) Start(ctx context.Context, opts agentsessions.StartOptions) (agentsessions.Session, error) {
	*p.started = true
	*p.profile = opts.Profile
	return p.Runtime.Start(ctx, opts)
}

// sandboxLaunch is what launchWithSandbox observed.
type sandboxLaunch struct {
	svc     *Service
	sessID  string
	err     error
	started bool
	profile sandbox.Profile
}

// launchWithSandbox creates a session for agent-1 and launches it against a
// catalog whose agent names sandboxName.
func launchWithSandbox(t *testing.T, sandboxName string) sandboxLaunch {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	wsRoot := t.TempDir()
	svc := &Service{
		CatalogRoot: t.TempDir(),
		Catalog: &config.Catalog{
			Global:          config.Global{Version: "test"},
			Agents:          map[string]config.Agent{"agent-1": {ID: "agent-1", Permissions: config.AgentPermissions{DefaultSandbox: sandboxName}}},
			SandboxProfiles: sandboxProfiles(),
		},
		Store: db,
	}
	const sessID = "sess-sandbox"
	plan := &launch.Plan{
		LaunchID: "launch-1", ProjectID: "proj-1", LogicalAgentID: "agent-1",
		ProviderID: "claude-pty", ProviderBrand: "claude", RuntimeKind: config.RuntimeKindPTY,
		RepoRoot: t.TempDir(), WriteHome: wsRoot, WorkspaceMode: "shared",
		Command: "echo", BootPrompt: "boot", BootMode: "stdin",
	}
	row := store.SessionRow{ID: sessID, LaunchID: "launch-1", ProjectID: "proj-1", LogicalAgentID: "agent-1",
		ProviderID: "claude-pty", ProviderKind: "cli", Workspace: wsRoot, State: string(session.StateCreated)}
	if err := db.CreateSession(row, plan); err != nil {
		t.Fatal(err)
	}
	var started bool
	var got sandbox.Profile
	svc.factories = map[string]RuntimeFactory{
		"claude-pty": func(_ *launch.Plan) (agentsessions.Runtime, error) {
			rt, err := stub.New(plan)
			return profileCapture{Runtime: rt, started: &started, profile: &got}, err
		},
	}
	mgr := agentsessions.NewManager(stateSinkAdapter{db: db})
	svc.Manager = mgr
	t.Cleanup(func() {
		for _, info := range mgr.List() {
			_ = mgr.Stop(context.Background(), info.ID)
		}
		_ = mgr.Shutdown(context.Background())
	})
	_, err = svc.LaunchSession(sessID)
	return sandboxLaunch{svc: svc, sessID: sessID, err: err, started: started, profile: got}
}

func TestLaunchSession_KnownSandboxProfileApplied(t *testing.T) {
	l := launchWithSandbox(t, "workspace-only")
	if l.err != nil {
		t.Fatalf("LaunchSession: %v", l.err)
	}
	if !l.started || l.profile.ID != "workspace-only" {
		t.Fatalf("runtime started=%v with profile %q; want workspace-only", l.started, l.profile.ID)
	}
}

func TestLaunchSession_EmptySandboxUnchanged(t *testing.T) {
	l := launchWithSandbox(t, "")
	if l.err != nil {
		t.Fatalf("LaunchSession: %v", l.err)
	}
	if !l.started || l.profile.ID != "" {
		t.Fatalf("runtime started=%v with profile %q; want no profile", l.started, l.profile.ID)
	}
}

// A session created while the profile existed, launched after the catalog
// lost it, is refused and marked failed; the runtime never starts.
func TestLaunchSession_UnknownSandboxProfileRefused(t *testing.T) {
	l := launchWithSandbox(t, "no-such-profile")
	if !errors.Is(l.err, config.ErrUnknownSandboxProfile) {
		t.Fatalf("LaunchSession err = %v; want ErrUnknownSandboxProfile", l.err)
	}
	if !strings.Contains(l.err.Error(), `"no-such-profile"`) {
		t.Fatalf("error %q does not name the profile", l.err)
	}
	if l.started {
		t.Fatal("runtime started without its sandbox")
	}
	row, gerr := l.svc.Store.GetSession(l.sessID)
	if gerr != nil {
		t.Fatal(gerr)
	}
	if row.State != string(session.StateFailed) {
		t.Fatalf("session state = %q; want failed", row.State)
	}
}
