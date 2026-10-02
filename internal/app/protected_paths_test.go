package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hollis-labs/agentkit/agentsessions"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/provider/acp"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/workspace"
)

func protectionOnForTest() ProtectionStatus { return ProtectionStatus{Enabled: true} }

// cliPlan is a launch of a CLI provider with no special handling.
var cliPlan = &launch.Plan{ProviderBrand: "claude"}

// envOf is a getenv over a fixed map.
func envOf(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

// tetherLayout builds a Tether root with catalog/, run/ and state/ under a
// temp dir, reached through a symlink, and returns a protected Service over
// it plus the canonical catalog and run paths.
func tetherLayout(t *testing.T) (svc *Service, catalog, run string) {
	t.Helper()
	// The guard these tests exercise is dormant in the shipped build (codex is
	// CodexNotProtected, CW-20261001-0230), so they switch it on; the tests of
	// the shipped state use tetherLayoutKeepingMode.
	setCodexProtectionMode(t, CodexGuarded)
	return tetherLayoutKeepingMode(t)
}

// tetherLayoutKeepingMode is tetherLayout with the codex protection mode left as
// the build ships it.
func tetherLayoutKeepingMode(t *testing.T) (svc *Service, catalog, run string) {
	t.Helper()
	base := t.TempDir()
	t.Setenv("HOME", base)
	root := filepath.Join(base, "tether")
	for _, d := range []string{"catalog", "run", "state"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(root, "catalog"), filepath.Join(base, ".tether")); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	cat := &config.Catalog{Global: config.Global{Version: "test"}}
	cat.Global.Daemon.PIDFile = filepath.Join(link, "run", "tetherd.pid")
	cat.Global.Daemon.ListenAddr = "unix:" + filepath.Join(link, "run", "tetherd.sock")
	cat.Global.Catalog.Defaults.StateDB = filepath.Join(link, "state", "tether.db")
	prev := bwrapAvailable
	bwrapAvailable = func(string) error { return nil }
	t.Cleanup(func() { bwrapAvailable = prev })
	svc = &Service{CatalogRoot: filepath.Join(link, "catalog"), Catalog: cat, protectionStatus: protectionOnForTest}
	return svc, filepath.Join(realRoot, "catalog"), filepath.Join(realRoot, "run")
}

// The catalog root, the daemon's run directory and the state database's
// directory are protected by their canonical paths, even when configured through
// a symlink.
func TestControlPlaneDirs(t *testing.T) {
	svc, catalog, run := tetherLayout(t)
	state := filepath.Join(filepath.Dir(catalog), "state")
	dirs, err := svc.controlPlaneDirs()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{catalog, run, state}; !slices.Equal(dirs, want) {
		t.Fatalf("dirs = %q, want %q", dirs, want)
	}

	// A pid file configured into a shared directory outside Tether's root
	// does not make that directory read-only for every agent.
	shared := t.TempDir()
	svc.Catalog.Global.Daemon.PIDFile = filepath.Join(shared, "tetherd.pid")
	svc.Catalog.Global.Daemon.ListenAddr = "tcp:127.0.0.1:0"
	if dirs, err = svc.controlPlaneDirs(); err != nil || !slices.Equal(dirs, []string{catalog, state}) {
		t.Fatalf("pid file outside the root: dirs = %q, %v; want the catalog and the state directory", dirs, err)
	}

	// A run directory that does not exist yet is skipped.
	svc.Catalog.Global.Daemon.PIDFile = filepath.Join(filepath.Dir(catalog), "absent", "tetherd.pid")
	if dirs, err = svc.controlPlaneDirs(); err != nil || !slices.Equal(dirs, []string{catalog, state}) {
		t.Fatalf("missing run dir: dirs = %q, %v; want the catalog and the state directory", dirs, err)
	}

	// The state directory is protected wherever it is, not only inside
	// Tether's root: it holds every session, message and event.
	elsewhere := t.TempDir()
	prevDB := svc.Catalog.Global.Catalog.Defaults.StateDB
	svc.Catalog.Global.Catalog.Defaults.StateDB = filepath.Join(elsewhere, "main.db")
	realElsewhere, err := filepath.EvalSymlinks(elsewhere)
	if err != nil {
		t.Fatal(err)
	}
	if dirs, err = svc.controlPlaneDirs(); err != nil || !slices.Equal(dirs, []string{catalog, realElsewhere}) {
		t.Fatalf("state db outside the root: dirs = %q, %v; want the catalog and %s", dirs, err, realElsewhere)
	}

	// A state directory that does not exist yet is skipped, like a run
	// directory: the daemon has nothing in it.
	svc.Catalog.Global.Catalog.Defaults.StateDB = filepath.Join(elsewhere, "absent", "main.db")
	if dirs, err = svc.controlPlaneDirs(); err != nil || !slices.Equal(dirs, []string{catalog}) {
		t.Fatalf("missing state dir: dirs = %q, %v; want only the catalog", dirs, err)
	}
	svc.Catalog.Global.Catalog.Defaults.StateDB = prevDB

	// A catalog root that does not resolve fails closed.
	svc.CatalogRoot = filepath.Join(t.TempDir(), "gone")
	if _, err := svc.controlPlaneDirs(); err == nil {
		t.Fatal("missing catalog root: want an error, got none")
	}
}

func TestControlPlaneDirs_ProtectEveryCatalogLayer(t *testing.T) {
	home, catalog, project, external := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	cat := &config.Catalog{Projects: map[string]config.Project{"p": {RepoRoot: project}}}
	cat.Global.Catalog.Roots.Agents = external
	svc := &Service{CatalogRoot: catalog, Catalog: cat}
	dirs, err := svc.controlPlaneDirs()
	if err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{catalog, filepath.Join(home, ".tether"), filepath.Join(project, ".tether"), external} {
		canonical, err := filepath.EvalSymlinks(root)
		if err != nil || !containsPath(dirs, canonical) {
			t.Fatalf("catalog layer left writable: %s (%v)", root, err)
		}
	}
	for _, outside := range []string{home, project} {
		if insideAnyRoot(dirs, outside) || containsPath(dirs, outside) {
			t.Fatal("layer protection swallowed workspace root", outside)
		}
	}
}

func TestApplyControlPlaneProtection(t *testing.T) {
	svc, catalog, run := tetherLayout(t)
	outside := t.TempDir()

	opts := agentsessions.StartOptions{Workdir: outside, WorkspaceDir: t.TempDir()}
	if err := svc.applyControlPlaneProtection(cliPlan, "cli", &opts); err != nil {
		t.Fatalf("launch outside the protected dirs: %v", err)
	}
	state := filepath.Join(filepath.Dir(catalog), "state")
	if want := []string{catalog, run, state}; !slices.Equal(opts.ProtectedPaths, want) {
		t.Fatalf("ProtectedPaths = %q, want %q", opts.ProtectedPaths, want)
	}

	for _, tc := range []struct {
		name    string
		opts    agentsessions.StartOptions
		stateDB string
		want    string
	}{
		{"workdir in catalog", agentsessions.StartOptions{Workdir: filepath.Join(catalog, "repo"), WorkspaceDir: outside}, "", "work directory"},
		{"workspace in run", agentsessions.StartOptions{Workdir: outside, WorkspaceDir: run}, "", "workspace"},
		{"workdir in state", agentsessions.StartOptions{Workdir: filepath.Join(state, "work"), WorkspaceDir: outside}, "", "work directory"},
		{"workspace in state", agentsessions.StartOptions{Workdir: outside, WorkspaceDir: state}, "", "workspace"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.stateDB != "" {
				prev := svc.Catalog.Global.Catalog.Defaults.StateDB
				svc.Catalog.Global.Catalog.Defaults.StateDB = tc.stateDB
				t.Cleanup(func() { svc.Catalog.Global.Catalog.Defaults.StateDB = prev })
			}
			opts := tc.opts
			err := svc.applyControlPlaneProtection(cliPlan, "cli", &opts)
			if !errors.Is(err, launch.ErrLaunchInsideProtectedPath) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v; want ErrLaunchInsideProtectedPath naming the %s", err, tc.want)
			}
			if opts.ProtectedPaths != nil {
				t.Fatalf("a refused launch still set ProtectedPaths %q", opts.ProtectedPaths)
			}
		})
	}

	// A state database inside the catalog no longer refuses a launch: the
	// agent does not write it (its `tether mcp` is daemon-only), and the catalog
	// is protected already.
	prevDB := svc.Catalog.Global.Catalog.Defaults.StateDB
	svc.Catalog.Global.Catalog.Defaults.StateDB = filepath.Join(catalog, "state.db")
	inCatalog := agentsessions.StartOptions{Workdir: outside, WorkspaceDir: outside}
	if err := svc.applyControlPlaneProtection(cliPlan, "cli", &inCatalog); err != nil {
		t.Fatalf("state db in the catalog: %v", err)
	}
	if want := []string{catalog, run}; !slices.Equal(inCatalog.ProtectedPaths, want) {
		t.Fatalf("state db in the catalog: ProtectedPaths = %q, want %q", inCatalog.ProtectedPaths, want)
	}
	svc.Catalog.Global.Catalog.Defaults.StateDB = prevDB

	// The in-process API stub starts no agent process.
	stubOpts := agentsessions.StartOptions{Workdir: outside}
	if err := svc.applyControlPlaneProtection(cliPlan, "api", &stubOpts); err != nil || stubOpts.ProtectedPaths != nil {
		t.Fatalf("api stub: ProtectedPaths = %q, err = %v; want none", stubOpts.ProtectedPaths, err)
	}

	// An ACP launch is refused while protection is on, naming the follow-up.
	acpOpts := agentsessions.StartOptions{Workdir: outside}
	err := svc.applyControlPlaneProtection(cliPlan, acp.Kind, &acpOpts)
	if !errors.Is(err, launch.ErrACPLaunchUnprotected) || !strings.Contains(err.Error(), "CW-20261001-0162") {
		t.Fatalf("acp: err = %v; want ErrACPLaunchUnprotected naming CW-20261001-0162", err)
	}

	// Turned off by the operator, or on a platform not yet covered, nothing
	// is applied or refused: launches run as they did before protection.
	for _, tc := range []struct {
		name   string
		status ProtectionStatus
	}{
		{"TETHER_SANDBOX_PROTECT=0", ControlPlaneProtection("linux", envOf(map[string]string{ProtectEnv: "0"}))},
		{"TETHER_SANDBOX_PROTECT=false", ControlPlaneProtection("linux", envOf(map[string]string{ProtectEnv: "false"}))},
		{"darwin", ControlPlaneProtection("darwin", envOf(nil))},
	} {
		t.Run("off/"+tc.name, func(t *testing.T) {
			status := tc.status
			svc.protectionStatus = func() ProtectionStatus { return status }
			t.Cleanup(func() { svc.protectionStatus = protectionOnForTest })
			offOpts := agentsessions.StartOptions{Workdir: filepath.Join(catalog, "repo")}
			if err := svc.applyControlPlaneProtection(cliPlan, "cli", &offOpts); err != nil || offOpts.ProtectedPaths != nil {
				t.Fatalf("ProtectedPaths = %q, err = %v; want none", offOpts.ProtectedPaths, err)
			}
			if err := svc.refuseUnprotectable(cliPlan, acp.Kind); err != nil {
				t.Fatalf("ACP refused: %v", err)
			}
		})
	}
}

// Protection applies on Linux unless the operator turns it off with
// TETHER_SANDBOX_PROTECT=0 or false; on darwin it is not applied until
// go-sandbox's seatbelt protection is verified (CW-20261001-0138).
func TestControlPlaneProtection(t *testing.T) {
	for _, tc := range []struct {
		goos, env  string
		enabled    bool
		byOperator bool
		reason     string
	}{
		{"linux", "", true, false, "on"},
		{"linux", "1", true, false, "on"},
		{"linux", "true", true, false, "on"},
		{"linux", "0", false, true, "DISABLED by TETHER_SANDBOX_PROTECT=0"},
		{"linux", "false", false, true, "DISABLED by TETHER_SANDBOX_PROTECT=false"},
		{"linux", " FALSE ", false, true, "DISABLED by TETHER_SANDBOX_PROTECT"},
		{"darwin", "", false, false, "CW-20261001-0138"},
		{"darwin", "0", false, true, "DISABLED by TETHER_SANDBOX_PROTECT=0"},
		{"windows", "", false, false, "not applied on windows"},
	} {
		got := ControlPlaneProtection(tc.goos, envOf(map[string]string{ProtectEnv: tc.env}))
		if got.Enabled != tc.enabled || got.DisabledByOperator != tc.byOperator || !strings.Contains(got.Reason, tc.reason) {
			t.Errorf("ControlPlaneProtection(%q, %s=%q) = %+v; want enabled=%v byOperator=%v reason containing %q",
				tc.goos, ProtectEnv, tc.env, got, tc.enabled, tc.byOperator, tc.reason)
		}
	}
}

// With protection on and bubblewrap missing or unable to build a namespace,
// every launch Tether must sandbox is refused before it starts, with the
// reason, the install hint and the switch, not run unprotected.
func TestRefuseUnprotectable_NoBwrap(t *testing.T) {
	svc, catalog, _ := tetherLayout(t)
	var probed string
	bwrapAvailable = func(dir string) error {
		probed = dir
		return errors.New("bwrap cannot build a protecting sandbox (exit status 1): bwrap: setting up uid map: Permission denied")
	}

	err := svc.refuseUnprotectable(cliPlan, "cli")
	if !errors.Is(err, launch.ErrProtectionUnavailable) || !strings.Contains(err.Error(), "install bubblewrap") ||
		!strings.Contains(err.Error(), ProtectEnv+"=0") || !strings.Contains(err.Error(), "setting up uid map") {
		t.Fatalf("refuseUnprotectable = %v; want ErrProtectionUnavailable with the cause, the install hint and the switch", err)
	}
	if probed != catalog {
		t.Fatalf("probed %q, want the catalog %q", probed, catalog)
	}
	opts := agentsessions.StartOptions{Workdir: t.TempDir()}
	if err := svc.applyControlPlaneProtection(cliPlan, "cli", &opts); !errors.Is(err, launch.ErrProtectionUnavailable) || opts.ProtectedPaths != nil {
		t.Fatalf("applyControlPlaneProtection = %v, ProtectedPaths = %q; want ErrProtectionUnavailable and none", err, opts.ProtectedPaths)
	}
	if err := svc.refuseUnprotectable(cliPlan, "api"); err != nil {
		t.Fatalf("api stub refused: %v", err)
	}
}

// fakeCodexWriter stands in for `codex exec`: each turn tries to create a
// file in the catalog, the run directory, the state directory and its own work
// directory, and reports which writes succeeded as its agent message.
const fakeCodexWriter = `#!/bin/sh
try() { if touch "$1/agent-wrote-this" 2>/dev/null; then echo wrote; else echo denied; fi; }
msg="catalog=$(try %q) run=$(try %q) state=$(try %q) work=$(try "$PWD")"
echo "{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"$msg\"}}"
echo '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}'
`

// startFakeCodex launches a stand-in codex (script) through LaunchSession and
// agentkit's adapter runtime on svc, with the given catalog flags, and returns
// the session id and workspace.
func startFakeCodex(t *testing.T, svc *Service, script string, args ...string) (string, *workspace.Session) {
	t.Helper()
	return startFakeCodexWith(t, svc, script, nil, args...)
}

// startFakeCodexWith is startFakeCodex with a hook to shape the launch plan
// first (caller environment, a repo root that is a symlink, an MCP allow-list).
func startFakeCodexWith(t *testing.T, svc *Service, script string, mod func(*launch.Plan), args ...string) (string, *workspace.Session) {
	t.Helper()
	sessID, ws, err := launchFakeCodex(t, svc, script, mod, args...)
	if err != nil {
		t.Fatalf("LaunchSession: %v", err)
	}
	return sessID, ws
}

// launchFakeCodex launches the stand-in and returns LaunchSession's error
// rather than failing the test, for a test that expects a refusal.
func launchFakeCodex(t *testing.T, svc *Service, script string, mod func(*launch.Plan), args ...string) (string, *workspace.Session, error) {
	t.Helper()
	fake := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil { //nolint:gosec // test stand-in must be executable
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	const sessID = "sess-protected"
	plan := &launch.Plan{
		LaunchID:       "codex-launch",
		ProjectID:      "proj",
		LogicalAgentID: "agent",
		ProviderID:     "codex-cli",
		ProviderBrand:  "codex",
		RuntimeKind:    config.RuntimeKindSubprocess,
		RepoRoot:       t.TempDir(),
		WriteHome:      t.TempDir(),
		WorkspaceMode:  "shared",
		Command:        fake,
		Args:           args,
	}
	if mod != nil {
		mod(plan)
	}
	ws, err := workspace.Create(plan.WriteHome, sessID, plan)
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	row := store.SessionRow{
		ID: sessID, LaunchID: plan.LaunchID, ProjectID: plan.ProjectID, LogicalAgentID: plan.LogicalAgentID,
		ProviderID: plan.ProviderID, ProviderKind: "cli", Workspace: ws.Root, State: "created",
	}
	if err := db.CreateSession(row, plan); err != nil {
		t.Fatalf("create session: %v", err)
	}
	factory, err := runtimeFactoryForProvider(config.Provider{ID: "codex-cli", Adapter: "codex", RuntimeKind: config.RuntimeKindSubprocess})
	if err != nil {
		t.Fatalf("runtime factory: %v", err)
	}
	svc.Store = db
	svc.Manager = agentsessions.NewManager(stateSinkAdapter{db: db})
	svc.factories = map[string]RuntimeFactory{"codex-cli": factory}
	if _, err := svc.LaunchSession(sessID); err != nil {
		return sessID, ws, err
	}
	t.Cleanup(func() { _ = svc.Manager.Stop(context.Background(), sessID) })
	return sessID, ws, nil
}

// CW-20261001-0142 acceptance, end to end through LaunchSession and
// agentkit's adapter runtime under bubblewrap: an agent launched by Tether
// cannot create a file in the catalog, the run directory or the state
// directory (CW-20261001-0173), can still write its own work directory, and
// the daemon can still write the catalog and the state directory. The
// stand-in is a codex launched with codex's own sandbox switched off, which
// is when Tether's protection applies to codex.
func TestProtectedLaunch_AgentCannotWriteCatalogOrRunDir(t *testing.T) {
	svc, catalog, run := tetherLayout(t)
	state := filepath.Join(filepath.Dir(catalog), "state")
	clearWritableRoots(t)
	if err := ProbeBwrap(catalog); err != nil {
		t.Skipf("bubblewrap cannot build a protecting sandbox on this host: %v", err)
	}
	sessID, ws := startFakeCodex(t, svc, fmt.Sprintf(fakeCodexWriter, catalog, run, state), "--sandbox", "danger-full-access")

	if err := svc.SendTurn(context.Background(), sessID, "write everywhere"); err != nil {
		t.Fatalf("turn: %v", err)
	}
	logData := waitForLog(t, ws.LogPath, "catalog=")
	if !strings.Contains(logData, "catalog=denied run=denied state=denied work=wrote") {
		t.Fatalf("agent writes = %q; want catalog, run and state denied, work dir written", logData)
	}
	for _, dir := range []string{catalog, run, state} {
		if _, err := os.Stat(filepath.Join(dir, "agent-wrote-this")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the agent created a file in %s (stat err = %v)", dir, err)
		}
	}
	for _, dir := range []string{catalog, state} {
		if err := os.WriteFile(filepath.Join(dir, "daemon-wrote-this"), []byte("ok"), 0o600); err != nil {
			t.Fatalf("the daemon can no longer write %s: %v", dir, err)
		}
	}
}

// The reviewer's scenario: protection is on and bubblewrap cannot build a
// namespace (AppArmor restricts unprivileged user namespaces), so it cannot
// nest either. A codex launch must still run, under codex's own sandbox,
// where every other provider is refused. The stand-in is not wrapped by
// Tether: it can see it is not in a private PID namespace.
func TestLaunchSession_CodexRunsWhereBwrapCannotNest(t *testing.T) {
	svc, _, _ := tetherLayout(t)
	clearWritableRoots(t)
	bwrapAvailable = func(string) error { return errors.New("bwrap: No permissions to create a new namespace") }

	if err := svc.refuseUnprotectable(cliPlan, "cli"); !errors.Is(err, launch.ErrProtectionUnavailable) {
		t.Fatalf("a claude launch here = %v; want ErrProtectionUnavailable", err)
	}
	const script = `#!/bin/sh
echo "{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"RAN pid1=$([ "$(cat /proc/1/comm)" = bwrap ] && echo yes || echo no)\"}}"
echo '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}'
`
	sessID, ws := startFakeCodex(t, svc, script)
	if err := svc.SendTurn(context.Background(), sessID, "go"); err != nil {
		t.Fatalf("a codex turn where bubblewrap cannot nest: %v", err)
	}
	if logData := waitForLog(t, ws.LogPath, "RAN"); !strings.Contains(logData, "RAN pid1=no") {
		t.Fatalf("log = %q; want the turn to run with no Tether sandbox around it", logData)
	}
}

// An ACP launch is refused at launch while protection is on: the ACP
// launcher cannot protect the catalog until CW-20261001-0162.
func TestLaunchSession_ACPRefusedWhileProtected(t *testing.T) {
	svc, _, _ := tetherLayout(t)
	factory, err := runtimeFactoryForProvider(config.Provider{ID: "copilot", Type: "cli", Command: "copilot", RuntimeKind: "acp-stdio"})
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	plan := &launch.Plan{LaunchID: "acp", ProjectID: "proj", ProviderID: "copilot", ProviderBrand: "copilot", RuntimeKind: "acp-stdio", WriteHome: t.TempDir(), RepoRoot: t.TempDir()}
	ws, err := workspace.Create(plan.WriteHome, "sess-acp", plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(store.SessionRow{ID: "sess-acp", LaunchID: "acp", ProjectID: "proj", ProviderID: "copilot", ProviderKind: acp.Kind, Workspace: ws.Root, State: "created"}, plan); err != nil {
		t.Fatal(err)
	}
	svc.Store = db
	svc.Manager = agentsessions.NewManager(stateSinkAdapter{db: db})
	svc.factories = map[string]RuntimeFactory{"copilot": factory}
	if _, err := svc.LaunchSession("sess-acp"); !errors.Is(err, launch.ErrACPLaunchUnprotected) {
		t.Fatalf("LaunchSession = %v, want ErrACPLaunchUnprotected", err)
	}
	got, err := db.GetSession("sess-acp")
	if err != nil || got.State != "failed" {
		t.Fatalf("session after refusal = %+v, %v; want failed", got, err)
	}
}

// codexPlan is a codex launch, as Tether resolves one from the catalog, with
// the given catalog flags.
func codexPlan(args ...string) *launch.Plan {
	return &launch.Plan{
		ProviderBrand:  "codex",
		RuntimeKind:    config.RuntimeKindSubprocess,
		PermissionMode: config.PermissionModeBypass,
		Args:           args,
	}
}

// clearWritableRoots takes /tmp and $TMPDIR out of codexOwnsSandbox's way,
// since a test's catalog lives under the test's own temp dir.
func clearWritableRoots(t *testing.T) {
	t.Helper()
	prev := codexExtraWritableRoots
	codexExtraWritableRoots = func([]string) []string { return nil }
	t.Cleanup(func() { codexExtraWritableRoots = prev })
}

// codexStartOpts is what LaunchSession has built by the time protection is
// applied: a work directory, a session workspace, and an environment whose
// CODEX_HOME is the boot dir planted under that workspace.
func codexStartOpts(t *testing.T) agentsessions.StartOptions {
	t.Helper()
	workspace, work := t.TempDir(), t.TempDir()
	boot := filepath.Join(workspace, "boot", "agentlaunch-bootdir-1")
	if err := os.MkdirAll(boot, 0o750); err != nil {
		t.Fatal(err)
	}
	return agentsessions.StartOptions{
		Workdir:      work,
		WorkspaceDir: workspace,
		Env:          []string{"PATH=/usr/bin", "CODEX_HOME=" + boot},
	}
}

// Codex runs inside its own workspace-write sandbox, which is bubblewrap too
// and cannot nest inside Tether's where unprivileged user namespaces are
// restricted. Tether does not wrap it and registers no ProtectedPaths for
// it, even where bubblewrap is unusable -- so it keeps working there -- but
// it still refuses a launch inside a protected directory.
func TestApplyControlPlaneProtection_CodexUsesItsOwnSandbox(t *testing.T) {
	svc, catalog, _ := tetherLayout(t)
	clearWritableRoots(t)
	bwrapAvailable = func(string) error { return errors.New("bwrap: setting up uid map: Permission denied") }

	opts := codexStartOpts(t)
	if err := svc.applyControlPlaneProtection(codexPlan(), "cli", &opts); err != nil || opts.ProtectedPaths != nil {
		t.Fatalf("codex: ProtectedPaths = %q, err = %v; want none and no refusal", opts.ProtectedPaths, err)
	}
	if err := svc.refuseUnprotectable(codexPlan(), "cli"); err != nil {
		t.Fatalf("codex create/resume probe refused: %v", err)
	}
	// Another provider, on the same host, is refused.
	if err := svc.refuseUnprotectable(cliPlan, "cli"); !errors.Is(err, launch.ErrProtectionUnavailable) {
		t.Fatalf("claude probe = %v; want ErrProtectionUnavailable", err)
	}

	inCatalog := codexStartOpts(t)
	inCatalog.Workdir = filepath.Join(catalog, "repo")
	if err := svc.applyControlPlaneProtection(codexPlan(), "cli", &inCatalog); !errors.Is(err, launch.ErrLaunchInsideProtectedPath) {
		t.Fatalf("codex with its work dir in the catalog = %v; want ErrLaunchInsideProtectedPath", err)
	}
}

// A project whose work root or repo root lies inside a protected directory is
// refused for every agent, not just the process directory: Codex's process
// cwd is its boot dir, and the project root is where it actually works.
func TestApplyControlPlaneProtection_RefusesProjectRootInsideProtectedDir(t *testing.T) {
	svc, catalog, run := tetherLayout(t)
	clearWritableRoots(t)
	for _, tc := range []struct {
		name string
		plan *launch.Plan
		set  func(*launch.Plan, string)
		dir  string
	}{
		{"claude repo root in catalog", &launch.Plan{ProviderBrand: "claude"}, func(p *launch.Plan, d string) { p.RepoRoot = d }, filepath.Join(catalog, "repo")},
		{"claude work root in run", &launch.Plan{ProviderBrand: "claude"}, func(p *launch.Plan, d string) { p.WorkRoot = d }, filepath.Join(run, "w")},
		{"codex repo root in catalog", codexPlan(), func(p *launch.Plan, d string) { p.RepoRoot = d }, filepath.Join(catalog, "repo")},
		{"codex work root in catalog", codexPlan(), func(p *launch.Plan, d string) { p.WorkRoot = d }, filepath.Join(catalog, "w")},
		{"claude repo root through a symlink into the catalog, to a path that does not exist yet", &launch.Plan{ProviderBrand: "claude"}, func(p *launch.Plan, d string) { p.RepoRoot = d }, filepath.Join(symlinkInto(t, catalog), "newsub")},
		{"codex work root through a symlink into the catalog, to a path that does not exist yet", codexPlan(), func(p *launch.Plan, d string) { p.WorkRoot = d }, filepath.Join(symlinkInto(t, catalog), "newsub")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.set(tc.plan, tc.dir)
			opts := codexStartOpts(t)
			err := svc.applyControlPlaneProtection(tc.plan, "cli", &opts)
			if !errors.Is(err, launch.ErrLaunchInsideProtectedPath) || !strings.Contains(err.Error(), "project") {
				t.Fatalf("err = %v; want ErrLaunchInsideProtectedPath naming the project root", err)
			}
		})
	}
}

// What a codex launch may carry and still be left to its own sandbox: the
// model, and overrides of the short list of keys that keep the sandbox as it
// is. Skills are native files that cannot write codex's config.
func TestCodexOwnsSandbox_KnownSafe(t *testing.T) {
	svc, _, _ := tetherLayout(t)
	clearWritableRoots(t)
	for _, tc := range []struct {
		name string
		mod  func(*launch.Plan)
		args []string
	}{
		{name: "no flags"},
		{name: "--model", args: []string{"--model", "gpt-5"}},
		{name: "-m", args: []string{"-m", "o3"}},
		{name: "--model=", args: []string{"--model=gpt-5-codex"}},
		{name: "-c model", args: []string{"-c", `model="gpt-5"`}},
		{name: "-c reasoning effort", args: []string{"-c", `model_reasoning_effort="high"`}},
		{name: "--config=", args: []string{`--config=approval_policy="on-request"`}},
		{name: "-c sandbox_mode workspace-write", args: []string{"-c", `sandbox_mode="workspace-write"`}},
		{name: "-c sandbox_mode read-only", args: []string{"-c", "sandbox_mode=read-only"}},
		{name: "skill native file", mod: func(p *launch.Plan) {
			p.NativeFiles = []launch.NativeFile{{Kind: "skill", ID: "review"}, {Kind: "raw", RelPath: "skills/x/SKILL.md"}}
		}},
		{name: "a project work root of its own", mod: func(p *launch.Plan) { p.RepoRoot = t.TempDir(); p.WorkRoot = t.TempDir() }},
		{name: "a project config above the project root, past its .git, is not the project's", mod: func(p *launch.Plan) {
			above := t.TempDir()
			writeFileT(t, filepath.Join(above, ".codex", "config.toml"), "x=1\n")
			repo := filepath.Join(above, "repo")
			if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o750); err != nil {
				t.Fatal(err)
			}
			p.RepoRoot = repo
		}},
		{name: "the user's own ~/.codex/config.toml is not a project config", mod: func(p *launch.Plan) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			writeFileT(t, filepath.Join(home, ".codex", "config.toml"), "model=\"x\"\n")
			proj := filepath.Join(home, "dev", "proj")
			if err := os.MkdirAll(proj, 0o750); err != nil {
				t.Fatal(err)
			}
			p.RepoRoot = proj
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := codexPlan(tc.args...)
			if tc.mod != nil {
				tc.mod(plan)
			}
			opts := codexStartOpts(t)
			dirs, outer, err := svc.protectionPlan(plan, "cli", &opts, false)
			if err != nil || outer || len(dirs) == 0 {
				t.Fatalf("outer = %v, dirs = %q, err = %v; want the exemption, with the dirs it would have protected", outer, dirs, err)
			}
		})
	}
}

// The denylist of the flags that name the weakening outright, the first line
// of defense the allowlist now backs: each puts Tether's protection back on
// the launch, with the catalog and run directory as the protected paths, and
// a flag that keeps codex confined is not one of them.
func TestApplyControlPlaneProtection_CodexSandboxWeakened(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"bypass flag", []string{"--dangerously-bypass-approvals-and-sandbox"}},
		{"yolo", []string{"--yolo"}},
		{"--sandbox", []string{"--sandbox", "danger-full-access"}},
		{"-s", []string{"-s", "danger-full-access"}},
		{"--sandbox=", []string{"--sandbox=danger-full-access"}},
		{"-c", []string{"-c", `sandbox_mode="danger-full-access"`}},
		{"--config", []string{"--config", "sandbox_mode=danger-full-access"}},
		{"--config=", []string{"--config=sandbox_mode='danger-full-access'"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, catalog, run := tetherLayout(t)
			state := filepath.Join(filepath.Dir(catalog), "state")
			clearWritableRoots(t)
			if !codexSandboxWeakened(tc.args) {
				t.Fatalf("codexSandboxWeakened(%q) = false", tc.args)
			}
			opts := codexStartOpts(t)
			if err := svc.applyControlPlaneProtection(codexPlan(tc.args...), "cli", &opts); err != nil {
				t.Fatal(err)
			}
			if want := []string{catalog, run, state}; !slices.Equal(opts.ProtectedPaths, want) {
				t.Fatalf("ProtectedPaths = %q, want Tether's protection %q", opts.ProtectedPaths, want)
			}
		})
	}
	for _, args := range [][]string{
		{"--sandbox", "workspace-write"}, {"-s", "read-only"}, {"-c", `model="gpt-5"`}, {"--model", "o3"},
	} {
		if codexSandboxWeakened(args) {
			t.Errorf("codexSandboxWeakened(%q) = true, want false", args)
		}
	}

	// A protected directory in or around /tmp or $TMPDIR, which codex's
	// sandbox makes writable, also puts Tether's protection back.
	svc, catalog, run := tetherLayout(t)
	prev := codexExtraWritableRoots
	codexExtraWritableRoots = func([]string) []string { return []string{"/nonexistent", filepath.Dir(filepath.Dir(catalog))} }
	t.Cleanup(func() { codexExtraWritableRoots = prev })
	opts := codexStartOpts(t)
	if err := svc.applyControlPlaneProtection(codexPlan(), "cli", &opts); err != nil {
		t.Fatal(err)
	}
	if want := []string{catalog, run, filepath.Join(filepath.Dir(catalog), "state")}; !slices.Equal(opts.ProtectedPaths, want) {
		t.Fatalf("protected dir under TMPDIR: ProtectedPaths = %q, want %q", opts.ProtectedPaths, want)
	}
}

// The exemption is an allowlist. Each way a launch can widen codex's own
// sandbox, every one of them demonstrated against canonical codex 0.159.x
// (CW-20261001-0142 review), puts Tether's protection back on the launch, as
// does anything the allowlist does not recognize. A caller with session.write
// reaches most of them through tether_session_create.
func TestCodexOwnsSandbox_BypassesAreWrapped(t *testing.T) {
	svc, catalog, _ := tetherLayout(t)
	clearWritableRoots(t)
	for _, tc := range []struct {
		name string
		args []string
		mod  func(*launch.Plan, *agentsessions.StartOptions)
	}{
		// Flags, operator or caller (agent_inline provider_overrides.extra_args).
		{name: "writable_roots via -c", args: []string{"-c", `sandbox_workspace_write.writable_roots=["` + catalog + `"]`}},
		{name: "writable_roots via --config=", args: []string{`--config=sandbox_workspace_write.writable_roots=["` + catalog + `"]`}},
		{name: "attached -csandbox_mode", args: []string{`-csandbox_mode="danger-full-access"`}},
		{name: "-c= form", args: []string{`-c=sandbox_mode="danger-full-access"`}},
		{name: "-s attached with =", args: []string{"-s=danger-full-access"}},
		{name: "-s attached value", args: []string{"-sdanger-full-access"}},
		{name: "-s danger-full-access", args: []string{"-s", "danger-full-access"}},
		{name: "--sandbox danger-full-access", args: []string{"--sandbox", "danger-full-access"}},
		{name: "profile sandbox_mode", args: []string{"-c", `profiles.p.sandbox_mode="danger-full-access"`, "-p", "p"}},
		{name: "profile alone", args: []string{"--profile", "p"}},
		{name: "relative --add-dir", args: []string{"--add-dir", "../catalog"}},
		{name: "absolute --add-dir", args: []string{"--add-dir", catalog}},
		{name: "--cd", args: []string{"--cd", "/"}},
		{name: "bypass flag", args: []string{"--dangerously-bypass-approvals-and-sandbox"}},
		{name: "--yolo", args: []string{"--yolo"}},
		{name: "unknown flag", args: []string{"--some-future-flag"}},
		{name: "positional", args: []string{"extra"}},
		{name: "-c with no value", args: []string{"-c"}},
		{name: "--model eating the next flag", args: []string{"--model", "-c"}},
		{name: "sandbox_mode via -c to a mode not on the list", args: []string{"-c", `sandbox_mode="danger-full-access"`}},
		{name: "unlisted -c key", args: []string{"-c", `shell_environment_policy.inherit="all"`}},
		// Injection: a boot dir overlay or raw native file can write config.toml
		// into CODEX_HOME.
		{name: "boot_dir_overlay config.toml", mod: func(p *launch.Plan, _ *agentsessions.StartOptions) {
			p.BootDirOverlay = map[string]string{"config.toml": "[sandbox_workspace_write]\nwritable_roots=[\"" + catalog + "\"]\n"}
		}},
		{name: "boot_dir_overlay anything", mod: func(p *launch.Plan, _ *agentsessions.StartOptions) {
			p.BootDirOverlay = map[string]string{"notes.md": "hi"}
		}},
		{name: "raw native file config.toml", mod: func(p *launch.Plan, _ *agentsessions.StartOptions) {
			p.NativeFiles = []launch.NativeFile{{Kind: "raw", RelPath: "config.toml"}}
		}},
		{name: "raw native file via ..", mod: func(p *launch.Plan, _ *agentsessions.StartOptions) {
			p.NativeFiles = []launch.NativeFile{{Kind: "raw", RelPath: "skills/../config.toml"}}
		}},
		{name: "raw native file .codex", mod: func(p *launch.Plan, _ *agentsessions.StartOptions) {
			p.NativeFiles = []launch.NativeFile{{Kind: "raw", RelPath: ".codex/config.toml"}}
		}},
		// Environment (override.env, provider_overrides.env).
		{name: "CODEX_EXEC_SERVER_URL", mod: func(_ *launch.Plan, o *agentsessions.StartOptions) {
			o.Env = append(o.Env, "CODEX_EXEC_SERVER_URL=http://127.0.0.1:1")
		}},
		{name: "other CODEX_ variable", mod: func(_ *launch.Plan, o *agentsessions.StartOptions) {
			o.Env = append(o.Env, "CODEX_SANDBOX_NETWORK_DISABLED=0")
		}},
		{name: "CODEX_HOME elsewhere", mod: func(_ *launch.Plan, o *agentsessions.StartOptions) {
			o.Env = []string{"CODEX_HOME=" + t.TempDir()}
		}},
		{name: "CODEX_HOME unset", mod: func(_ *launch.Plan, o *agentsessions.StartOptions) {
			o.Env = []string{"PATH=/usr/bin"}
		}},
		// Where codex runs.
		{name: "project .codex/config.toml in the work dir", mod: func(_ *launch.Plan, o *agentsessions.StartOptions) {
			writeFileT(t, filepath.Join(o.Workdir, ".codex", "config.toml"), "x=1\n")
		}},
		{name: "project .codex/config.toml above the work dir", mod: func(_ *launch.Plan, o *agentsessions.StartOptions) {
			sub := filepath.Join(o.Workdir, "a", "b")
			if err := os.MkdirAll(sub, 0o750); err != nil {
				t.Fatal(err)
			}
			writeFileT(t, filepath.Join(o.Workdir, ".codex", "config.toml"), "x=1\n")
			o.Workdir = sub
		}},
		{name: "work dir containing the catalog", mod: func(_ *launch.Plan, o *agentsessions.StartOptions) {
			o.Workdir = filepath.Dir(catalog)
		}},
		{name: "work dir far above the catalog", mod: func(_ *launch.Plan, o *agentsessions.StartOptions) {
			o.Workdir = filepath.Dir(filepath.Dir(catalog))
		}},
		{name: "workspace containing the catalog", mod: func(_ *launch.Plan, o *agentsessions.StartOptions) {
			o.WorkspaceDir = filepath.Dir(catalog)
			o.Env = []string{"CODEX_HOME=" + filepath.Join(filepath.Dir(catalog), "boot")}
		}},
		// Codex's process cwd is its boot dir; its working directory (--cd) is the
		// project's work root, and that is where its sandbox and project config
		// come from. The live probe wrote the catalog through exactly this.
		{name: "project .codex/config.toml in the repo root (process cwd is the boot dir)", mod: func(p *launch.Plan, _ *agentsessions.StartOptions) {
			root := t.TempDir()
			writeFileT(t, filepath.Join(root, ".codex", "config.toml"), "x=1\n")
			p.RepoRoot = root
		}},
		{name: "project .codex/config.toml in the work root", mod: func(p *launch.Plan, _ *agentsessions.StartOptions) {
			root := t.TempDir()
			writeFileT(t, filepath.Join(root, ".codex", "config.toml"), "x=1\n")
			p.WorkRoot = root
		}},
		{name: "project repo root containing the catalog", mod: func(p *launch.Plan, _ *agentsessions.StartOptions) {
			p.RepoRoot = filepath.Dir(catalog)
		}},
		{name: "project work root far above the catalog", mod: func(p *launch.Plan, _ *agentsessions.StartOptions) {
			p.WorkRoot = filepath.Dir(filepath.Dir(catalog))
		}},
		// The posture the exemption rests on.
		{name: "unknown runtime kind", mod: func(p *launch.Plan, _ *agentsessions.StartOptions) { p.RuntimeKind = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := codexPlan(tc.args...)
			opts := codexStartOpts(t)
			if tc.mod != nil {
				tc.mod(plan, &opts)
			}
			ok, why := codexOwnsSandbox(plan, &opts, []string{catalog})
			if ok || why == "" {
				t.Fatalf("codexOwnsSandbox = %v, %q; want it refused with a reason, so the launch is wrapped", ok, why)
			}
			// And through the canonical decision: Tether wraps it, where it can.
			_, outer, err := svc.protectionPlan(plan, "cli", &opts, false)
			if err != nil || !outer {
				t.Fatalf("protectionPlan outer = %v, err = %v; want Tether's sandbox on the launch", outer, err)
			}
		})
	}
}

// A caller reaches the flags through agent_inline, whose
// provider_overrides.<id>.extra_args are appended to the plan's args by
// applyProviderOverrides, and the environment through the same block. The
// canonical function builds the plan here, so the test follows the canonical path.
func TestCodexOwnsSandbox_CallerSuppliedOverrides(t *testing.T) {
	svc, catalog, _ := tetherLayout(t)
	clearWritableRoots(t)
	for _, extra := range [][]string{
		{"-c", `sandbox_workspace_write.writable_roots=["` + catalog + `"]`},
		{"--add-dir", catalog},
		{`-csandbox_mode="danger-full-access"`},
	} {
		plan := codexPlan("--model", "gpt-5") // the operator's own, safe, catalog flags
		plan.ProviderID = "codex-cli"
		applyProviderOverrides(plan, map[string]config.ProviderOverride{"codex-cli": {ExtraArgs: extra}})
		opts := codexStartOpts(t)
		if _, outer, err := svc.protectionPlan(plan, "cli", &opts, false); err != nil || !outer {
			t.Errorf("caller extra_args %q: outer = %v, err = %v; want the launch wrapped", extra, outer, err)
		}
	}

	plan := codexPlan()
	plan.ProviderID = "codex-cli"
	applyProviderOverrides(plan, map[string]config.ProviderOverride{"codex-cli": {Env: map[string]string{"CODEX_EXEC_SERVER_URL": "http://127.0.0.1:1"}}})
	opts := codexStartOpts(t)
	for k, v := range plan.Env { // BuildEnv puts a launch's env into the agent's environment
		opts.Env = append(opts.Env, k+"="+v)
	}
	if _, outer, err := svc.protectionPlan(plan, "cli", &opts, false); err != nil || !outer {
		t.Errorf("caller env CODEX_EXEC_SERVER_URL: outer = %v, err = %v; want the launch wrapped", outer, err)
	}
}

// A codex launch the allowlist refuses is wrapped, and where Tether's
// sandbox cannot start it is refused: the fail-closed outcome, loud, never
// silently unprotected.
func TestCodexOwnsSandbox_WrappedLaunchFailsClosedWhereBwrapCannotNest(t *testing.T) {
	svc, catalog, _ := tetherLayout(t)
	clearWritableRoots(t)
	bwrapAvailable = func(string) error { return errors.New("bwrap: No permissions to create a new namespace") }
	opts := codexStartOpts(t)
	plan := codexPlan("-c", `sandbox_workspace_write.writable_roots=["`+catalog+`"]`)
	err := svc.applyControlPlaneProtection(plan, "cli", &opts)
	if !errors.Is(err, launch.ErrProtectionUnavailable) || opts.ProtectedPaths != nil {
		t.Fatalf("apply = %v, ProtectedPaths = %q; want ErrProtectionUnavailable and nothing unprotected", err, opts.ProtectedPaths)
	}
	if err := svc.refuseUnprotectable(plan, "cli"); !errors.Is(err, launch.ErrProtectionUnavailable) {
		t.Fatalf("create/resume probe = %v; want ErrProtectionUnavailable", err)
	}
}

// Codex's flags are judged by name, never by value, in the log line that
// says why a launch was wrapped: a value may be a secret.
func TestCodexUnrecognisedArg_NamesTheFlagNotItsValue(t *testing.T) {
	for _, args := range [][]string{
		{"--api-key=sk-secret-value-1234"},
		{"-c", `model_provider.api_key="sk-secret-value-1234"`},
		{"--config=model_provider.api_key=sk-secret-value-1234"},
	} {
		flag, bad := codexUnrecognisedArg(args)
		if !bad || strings.Contains(flag, "sk-secret") {
			t.Errorf("codexUnrecognisedArg(%q) = %q, %v; want the flag named without its value", args, flag, bad)
		}
	}
}

func writeFileT(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Real codex honors its cwd as a writable root: a codex whose cwd contains
// the protected directory can write it, the premise of the overlap check. It
// runs `codex sandbox`, which needs no model, on a temp CODEX_HOME fixture
// and never the canonical ~/.codex, and skips where codex is absent or cannot
// build its sandbox. Opt in with TETHER_TEST_REAL_CODEX_SANDBOX=1; normal
// unit runs never discover or execute the host's codex binary.
func TestRealCodex_WorkspaceSandboxFollowsTheCwd(t *testing.T) {
	if os.Getenv("TETHER_TEST_REAL_CODEX_SANDBOX") != "1" {
		t.Skip("set TETHER_TEST_REAL_CODEX_SANDBOX=1 to run the canonical codex sandbox probe")
	}
	bin, err := exec.LookPath("codex")
	if err != nil {
		t.Skip("codex is not installed")
	}
	root := t.TempDir()
	catalog, work := filepath.Join(root, "catalog"), filepath.Join(root, "work")
	for _, d := range []string{catalog, work} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	touch := func(cwd, target string) error {
		cmd := exec.Command(bin, "sandbox", "-P", ":workspace", "-C", cwd, "--", "touch", target) //nolint:gosec // G204: fixed argv, test fixture paths
		cmd.Env = append(os.Environ(), "CODEX_HOME="+t.TempDir(), "TMPDIR="+t.TempDir())
		return cmd.Run()
	}
	if err := touch(work, filepath.Join(work, "ok")); err != nil {
		t.Skipf("codex cannot build its sandbox on this host: %v", err)
	}
	if err := touch(work, filepath.Join(catalog, "x")); err == nil {
		t.Errorf("codex with cwd %s wrote %s; its sandbox should make it read-only", work, catalog)
	}
	if err := touch(root, filepath.Join(catalog, "y")); err != nil {
		t.Errorf("codex with cwd %s (above the catalog) could not write it: %v; the overlap check assumes it can", root, err)
	}
}

// TMPDIR comes from the agent's environment, ahead of the daemon's.
func TestCodexExtraWritableRoots(t *testing.T) {
	t.Setenv("TMPDIR", "/daemon/tmp")
	if got := codexExtraWritableRoots(nil); !slices.Equal(got, []string{"/tmp", "/daemon/tmp"}) {
		t.Errorf("roots = %q", got)
	}
	if got := codexExtraWritableRoots([]string{"A=1", "TMPDIR=/agent/tmp"}); !slices.Equal(got, []string{"/tmp", "/agent/tmp"}) {
		t.Errorf("roots = %q", got)
	}
}

// The run directory is protected when it lies in a Tether root: the
// catalog's parent, or ~/.tether even when the catalog lives elsewhere.
func TestControlPlaneDirs_RunDirIndependentOfTheCatalogParent(t *testing.T) {
	home, other := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	for _, d := range []string{filepath.Join(home, ".tether", "run"), filepath.Join(home, "shared"), filepath.Join(other, "catalog")} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	realHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	realOther, err := filepath.EvalSymlinks(other)
	if err != nil {
		t.Fatal(err)
	}
	cat := &config.Catalog{Global: config.Global{Version: "test"}}
	cat.Global.Daemon.PIDFile = filepath.Join(home, ".tether", "run", "tetherd.pid")
	cat.Global.Daemon.ListenAddr = "unix:" + filepath.Join(home, "shared", "tetherd.sock")
	svc := &Service{CatalogRoot: filepath.Join(other, "catalog"), Catalog: cat}

	dirs, err := svc.controlPlaneDirs()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(realOther, "catalog"), filepath.Join(realHome, ".tether"), filepath.Join(realHome, ".tether", "run")}
	if !slices.Equal(dirs, want) {
		t.Fatalf("dirs = %q, want %q: ~/.tether/run protected although the catalog is elsewhere, the socket in an unrelated shared dir not", dirs, want)
	}
}

// ComputeProtectionHealth probes the host only when protection is on, on
// Linux, and reports why a probe failed.
func TestComputeProtectionHealth(t *testing.T) {
	probe := func(err error) (func(string) error, *string) {
		var got string
		return func(dir string) error { got = dir; return err }, &got
	}
	on := ProtectionStatus{Enabled: true, Reason: "on"}

	p, dir := probe(nil)
	if h := ComputeProtectionHealth(on, "linux", "/c", p); !h.BwrapChecked || !h.BwrapUsable || h.BwrapError != "" || *dir != "/c" {
		t.Errorf("usable host: %+v, probed %q", h, *dir)
	}
	p, _ = probe(errors.New("setting up uid map: Permission denied"))
	if h := ComputeProtectionHealth(on, "linux", "/c", p); !h.BwrapChecked || h.BwrapUsable || !strings.Contains(h.BwrapError, "uid map") {
		t.Errorf("unusable host: %+v", h)
	}
	if h := ComputeProtectionHealth(on, "linux", "", p); h.BwrapUsable || h.BwrapError == "" {
		t.Errorf("no catalog root: %+v", h)
	}
	for name, h := range map[string]ProtectionHealth{
		"off":    ComputeProtectionHealth(ProtectionStatus{DisabledByOperator: true}, "linux", "/c", p),
		"darwin": ComputeProtectionHealth(on, "darwin", "/c", p),
	} {
		if h.BwrapChecked {
			t.Errorf("%s: probed the host: %+v", name, h)
		}
	}
}

// symlinkInto returns a new symlink, in its own temp dir, to dir.
func symlinkInto(t *testing.T, dir string) string {
	t.Helper()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	return link
}
