package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/agentkit/agentsessions"

	"github.com/hollis-labs/tether/internal/launch"
)

// setCodexProtectionMode drives the switch for one test and restores it.
func setCodexProtectionMode(t *testing.T, mode CodexProtection) {
	t.Helper()
	prev := codexProtectionMode
	codexProtectionMode = mode
	t.Cleanup(func() { codexProtectionMode = prev })
}

// /health and `mux doctor` report how codex is protected, honestly in both
// modes, and name CW-20261001-0230, the structural reason: codex spawns MCP
// servers outside its sandbox.
func TestCodexProtectionState(t *testing.T) {
	on := ProtectionStatus{Enabled: true}

	// The build ships codex as not protected: the state a test sees before it
	// drives the switch is the state an operator sees.
	got := codexProtectionState(on)
	if got.State != "not protected" || !strings.Contains(got.Reason, "not protected (CW-20261001-0230)") ||
		!strings.Contains(got.Reason, "outside that sandbox") || !strings.Contains(got.Reason, "still refused") ||
		!strings.Contains(got.Reason, "torque_session_launch") || !strings.Contains(got.Reason, "loom_export_bundle") ||
		!strings.Contains(got.Reason, "Claude and OpenCode agents are protected") {
		t.Fatalf("shipped (not-protected) state = %+v", got)
	}

	setCodexProtectionMode(t, CodexGuarded)
	got = codexProtectionState(on)
	if got.State != "guarded" || !strings.Contains(got.Reason, "CW-20261001-0230") ||
		!strings.Contains(got.Reason, "every launch") || !strings.Contains(got.Reason, "can reach the catalog") {
		t.Fatalf("guarded state = %+v", got)
	}

	// With control-plane protection off, nothing is protected, codex included,
	// whichever mode: the state says so rather than "guarded".
	for _, mode := range []CodexProtection{CodexGuarded, CodexNotProtected} {
		setCodexProtectionMode(t, mode)
		if got := codexProtectionState(ProtectionStatus{}); got.State != "not applicable" {
			t.Fatalf("protection off, mode %q: state = %+v", mode, got)
		}
	}

	// The health report carries it, and the protection reason is honest about
	// codex in each mode.
	setCodexProtectionMode(t, CodexNotProtected)
	h := ComputeProtectionHealth(on, "linux", "/c", func(string) error { return nil })
	if h.Codex.State != "not protected" {
		t.Fatalf("ComputeProtectionHealth codex = %+v", h.Codex)
	}
	if r := ControlPlaneProtection("linux", func(string) string { return "" }).Reason; !strings.Contains(r, "Codex is NOT protected (CW-20261001-0230)") {
		t.Fatalf("protection reason hides that codex is unprotected: %q", r)
	}
	setCodexProtectionMode(t, CodexGuarded)
	if r := ControlPlaneProtection("linux", func(string) string { return "" }).Reason; strings.Contains(r, "NOT protected") || !strings.Contains(r, "MCP servers run outside that sandbox (CW-20261001-0230)") {
		t.Fatalf("guarded protection reason = %q; want no \"NOT protected\" and the MCP caveat naming CW-20261001-0230", r)
	}
}

// The state the build ships: codex runs as it did on main. Under the default,
// with Tether's sandbox unable to start on this host, a codex launch that the
// dormant guard would wrap or refuse (a caller PATH, a widening flag, a project
// .codex/config.toml, a work root containing the catalog) launches unwrapped,
// registers no per-turn check, and every turn runs, including one after a
// project config appears. claude on the same host is still refused.
func TestCodexProtectionMode_ShippedDefaultRunsCodexAsOnMain(t *testing.T) {
	if codexProtectionMode != CodexNotProtected {
		t.Fatalf("the build ships codexProtectionMode = %q; want %q while CW-20261001-0230 is open", codexProtectionMode, CodexNotProtected)
	}
	svc, catalog, _ := tetherLayoutKeepingMode(t)
	clearWritableRoots(t)
	bwrapAvailable = func(string) error { return os.ErrPermission } // Tether's sandbox cannot start

	counter := filepath.Join(t.TempDir(), "runs")
	script := "#!/bin/sh\necho run >> \"" + counter + "\"\necho '{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"RAN\"}}'\necho '{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}'\n"
	sessID, _, err := launchFakeCodex(t, svc, script, func(p *launch.Plan) {
		p.CallerEnv = []string{"PATH"}
		p.Env = map[string]string{launch.MCPServersEnv: "nanite"}
		p.WorkRoot = filepath.Dir(catalog)
	}, "-c", `sandbox_workspace_write.writable_roots=["`+catalog+`"]`)
	if err != nil {
		t.Fatalf("a codex launch the dormant guard would refuse was refused in the shipped state: %v", err)
	}
	if _, registered := svc.codexExempt.Load(sessID); registered {
		t.Fatal("the shipped state registered a per-turn check")
	}
	plan, err := svc.Store.GetLaunchPlan(sessID)
	if err != nil {
		t.Fatal(err)
	}
	writeFileT(t, filepath.Join(plan.RepoRoot, ".codex", "config.toml"), "[sandbox_workspace_write]\nwritable_roots=[\"/c\"]\n")
	for i := 0; i < 2; i++ {
		if err := svc.SendTurn(context.Background(), sessID, "go"); err != nil {
			t.Fatalf("turn %d refused in the shipped state: %v", i+1, err)
		}
	}
	deadline := time.Now().Add(10 * time.Second)
	runs := func() int {
		data, _ := os.ReadFile(counter) //nolint:gosec // test-owned path
		return strings.Count(string(data), "run")
	}
	for runs() < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := runs(); got != 2 {
		t.Fatalf("the agent ran %d times, want 2", got)
	}

	// claude, on the same host, is still protected: refused where Tether's
	// sandbox cannot start.
	if err := svc.refuseUnprotectable(cliPlan, "cli"); err == nil {
		t.Fatal("the shipped state left claude unprotected")
	}
}

// The fallback: with the switch set, codex runs exactly as it did before
// protection existed. None of the guard applies to it (no allowlist, no
// wrapping, no refusal), every shape the guard would have wrapped is left to
// codex's own sandbox, even where Tether's sandbox cannot start, and nothing is
// re-checked per turn. Everything else is unchanged: claude stays protected, and
// the planted mux server still refuses to write the catalog.
func TestCodexProtectionMode_FallbackRunsCodexAsBefore(t *testing.T) {
	svc, catalog, run := tetherLayout(t)
	clearWritableRoots(t)
	setCodexProtectionMode(t, CodexNotProtected)
	bwrapAvailable = func(string) error { return os.ErrPermission } // Tether's sandbox cannot start

	for name, shape := range map[string]func(*launch.Plan, *agentsessions.StartOptions){
		"a writable_roots flag": func(p *launch.Plan, _ *agentsessions.StartOptions) {
			p.Args = []string{"-c", `sandbox_workspace_write.writable_roots=["` + catalog + `"]`}
		},
		"caller environment": func(p *launch.Plan, _ *agentsessions.StartOptions) { p.CallerEnv = []string{"PATH"} },
		"nanite on the MCP list": func(p *launch.Plan, _ *agentsessions.StartOptions) {
			p.Env = map[string]string{launch.MCPServersEnv: "nanite"}
		},
		"a project .codex/config.toml": func(p *launch.Plan, _ *agentsessions.StartOptions) {
			root := t.TempDir()
			writeFileT(t, filepath.Join(root, ".codex", "config.toml"), "x=1\n")
			p.RepoRoot = root
		},
		"a work root containing the catalog": func(p *launch.Plan, _ *agentsessions.StartOptions) { p.WorkRoot = filepath.Dir(catalog) },
	} {
		t.Run(name, func(t *testing.T) {
			plan := codexPlan()
			opts := codexStartOpts(t)
			shape(plan, &opts)
			if err := svc.applyControlPlaneProtection(plan, "cli", &opts); err != nil || opts.ProtectedPaths != nil {
				t.Fatalf("fallback apply: ProtectedPaths = %q, err = %v; want codex left alone", opts.ProtectedPaths, err)
			}
			if err := svc.refuseUnprotectable(plan, "cli"); err != nil {
				t.Fatalf("fallback create/resume probe refused: %v", err)
			}
			if ex := svc.codexExemptionFor(plan, "cli", &opts); ex != nil {
				t.Fatal("the fallback registered a per-turn check")
			}
		})
	}

	// claude, on the same host, is still protected, and refused where Tether's
	// sandbox cannot start.
	if err := svc.refuseUnprotectable(cliPlan, "cli"); err == nil {
		t.Fatal("the fallback unprotected claude too")
	}
	bwrapAvailable = func(string) error { return nil }
	opts := codexStartOpts(t)
	if err := svc.applyControlPlaneProtection(cliPlan, "cli", &opts); err != nil || len(opts.ProtectedPaths) != 3 || opts.ProtectedPaths[0] != catalog || opts.ProtectedPaths[1] != run || opts.ProtectedPaths[2] != filepath.Join(filepath.Dir(catalog), "state") {
		t.Fatalf("claude under the fallback: ProtectedPaths = %q, err = %v; want the catalog, run and state dirs", opts.ProtectedPaths, err)
	}
	// And so is opencode, which has no special handling.
	opencode := &launch.Plan{ProviderBrand: "opencode"}
	opts = codexStartOpts(t)
	if err := svc.applyControlPlaneProtection(opencode, "cli", &opts); err != nil || len(opts.ProtectedPaths) != 3 {
		t.Fatalf("opencode under the fallback: ProtectedPaths = %q, err = %v", opts.ProtectedPaths, err)
	}
}

// Through LaunchSession, in the fallback: the planted mux server is still told
// to refuse writing the catalog (that policy does not depend on the switch), a
// codex session is not given a per-turn check, and a project .codex/config.toml
// planted between turns is not refused (the guard is off, as on main). In
// the guarded mode the same sequence is refused: the control for this test.
func TestCodexProtectionMode_FallbackThroughLaunchSession(t *testing.T) {
	script := func(counter string) string {
		return "#!/bin/sh\necho run >> \"" + counter + "\"\necho '{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"RAN\"}}'\necho '{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}'\n"
	}
	run := func(t *testing.T, mode CodexProtection) (refused bool, plantedProtect bool) {
		t.Helper()
		svc, catalog, _ := tetherLayout(t)
		clearWritableRoots(t)
		setCodexProtectionMode(t, mode)
		counter := filepath.Join(t.TempDir(), "runs")
		sessID, ws := startFakeCodex(t, svc, script(counter))
		matches, _ := filepath.Glob(filepath.Join(ws.Root, "boot", "agentlaunch-bootdir-*", "config.toml"))
		if len(matches) == 1 {
			data, _ := os.ReadFile(matches[0]) //nolint:gosec // test-owned path
			plantedProtect = strings.Contains(string(data), `"--protect-path", "`+catalog+`"`)
		}
		plan, err := svc.Store.GetLaunchPlan(sessID)
		if err != nil {
			t.Fatal(err)
		}
		writeFileT(t, filepath.Join(plan.RepoRoot, ".codex", "config.toml"), "[sandbox_workspace_write]\nwritable_roots=[\"/c\"]\n")
		err = svc.SendTurn(context.Background(), sessID, "go")
		return err != nil, plantedProtect
	}

	refused, planted := run(t, CodexNotProtected)
	if refused || !planted {
		t.Fatalf("fallback: turn refused = %v, planted --protect-path = %v; want the turn allowed (guard off) and the mux server still told to protect the catalog", refused, planted)
	}
	refused, planted = run(t, CodexGuarded)
	if !refused || !planted {
		t.Fatalf("guarded (control): turn refused = %v, planted --protect-path = %v; want the planted project config refused", refused, planted)
	}
}

// Even the dormant Codex agent guard requires a confined planted proxy.
func TestMCPProtectedPaths_UnnameableDirsFailEveryProtectedLaunch(t *testing.T) {
	svc, _, _ := tetherLayoutKeepingMode(t)
	svc.CatalogRoot = filepath.Join(t.TempDir(), "no-such-catalog")
	codex := &launch.Plan{LaunchID: "c", ProviderBrand: "codex"}

	if dirs, err := svc.mcpProtectedPaths(codex); err == nil || dirs != nil {
		t.Fatalf("shipped codex: dirs = %q, err = %v; want no dirs and an error", dirs, err)
	}
	if _, err := svc.mcpProtectedPaths(cliPlan); err == nil {
		t.Fatal("claude's launch did not fail when the protected directories cannot be named")
	}
	setCodexProtectionMode(t, CodexGuarded)
	if _, err := svc.mcpProtectedPaths(codex); err == nil {
		t.Fatal("guarded codex's launch did not fail when the protected directories cannot be named")
	}

	// With the directories nameable, shipped codex still gets them.
	good, catalog, run := tetherLayoutKeepingMode(t)
	setCodexProtectionMode(t, CodexNotProtected)
	if dirs, err := good.mcpProtectedPaths(codex); err != nil || len(dirs) != 3 || dirs[0] != catalog || dirs[1] != run || dirs[2] != filepath.Join(filepath.Dir(catalog), "state") {
		t.Fatalf("shipped codex with a resolvable catalog: dirs = %q, err = %v; want the catalog, run and state dirs", dirs, err)
	}
}
