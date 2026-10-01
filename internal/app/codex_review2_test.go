package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/bootgen"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launch"
)

// Caller-supplied environment cannot be enumerated into a denylist: PATH
// shadowing bwrap swaps codex's read-only root for a writable one, a relative
// TMPDIR is resolved by codex against its own cwd, an ancestor TMPDIR lets the
// agent plant CODEX_HOME/config.toml, LD_PRELOAD runs code inside it. Every one
// of them keeps a plain launch exempt and defeats codex's sandbox (CW-20261001-
// 0142 review, real codex 0.159.3). So a launch is left to codex's own sandbox
// only when NO environment came from a caller or an agent definition, and the
// plan records which keys did, through the real functions that merge them.
func TestCodexOwnsSandbox_CallerEnvIsNeverTrusted(t *testing.T) {
	svc, catalog, _ := tetherLayout(t)
	clearWritableRoots(t)
	overrides := func(env map[string]string) map[string]config.ProviderOverride {
		return map[string]config.ProviderOverride{"codex-cli": {Env: env}}
	}
	for _, tc := range []struct {
		name  string
		shape func(t *testing.T, p *launch.Plan)
		key   string
	}{
		{"PATH shadowing bwrap, from agent_inline provider_overrides", func(_ *testing.T, p *launch.Plan) {
			applyProviderOverrides(p, overrides(map[string]string{"PATH": "/tmp/fake-bin:/usr/bin"}))
		}, "PATH"},
		{"a relative TMPDIR, from override.env", func(t *testing.T, p *launch.Plan) {
			if _, err := applyOverride(p, "prompt", `{"env":{"TMPDIR":"../../catalog"}}`); err != nil {
				t.Fatal(err)
			}
		}, "TMPDIR"},
		{"an absolute TMPDIR, from override.env", func(t *testing.T, p *launch.Plan) {
			if _, err := applyOverride(p, "prompt", `{"env":{"TMPDIR":"/var/tmp"}}`); err != nil {
				t.Fatal(err)
			}
		}, "TMPDIR"},
		{"LD_PRELOAD", func(_ *testing.T, p *launch.Plan) {
			applyProviderOverrides(p, overrides(map[string]string{"LD_PRELOAD": "/tmp/x.so"}))
		}, "LD_PRELOAD"},
		{"a harmless variable: there is no caller environment at all", func(_ *testing.T, p *launch.Plan) {
			applyProviderOverrides(p, overrides(map[string]string{"FOO": "bar"}))
		}, "FOO"},
		{"an agent definition's own provider_overrides env (a layer an agent can write)", func(_ *testing.T, p *launch.Plan) {
			// The catalog agent's block reaches the plan through the same merge.
			applyProviderOverrides(p, overrides(map[string]string{"HOME": "/elsewhere"}))
		}, "HOME"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := codexPlan()
			plan.ProviderID = "codex-cli"
			tc.shape(t, plan)
			if !contains(plan.CallerEnv, tc.key) {
				t.Fatalf("plan.CallerEnv = %v; the merge did not record %s", plan.CallerEnv, tc.key)
			}
			opts := codexStartOpts(t)
			for k, v := range plan.Env { // BuildEnv puts the plan's env into the agent's
				opts.Env = append(opts.Env, k+"="+v)
			}
			ok, why := codexOwnsSandbox(plan, &opts, []string{catalog})
			if ok || !strings.Contains(why, tc.key) {
				t.Fatalf("codexOwnsSandbox = %v, %q; want it refused, naming %s", ok, why, tc.key)
			}
			if strings.Contains(why, "/tmp/fake-bin") || strings.Contains(why, "x.so") {
				t.Fatalf("the reason echoes a value: %q", why)
			}
			if _, outer, err := svc.protectionPlan(plan, "cli", &opts, false); err != nil || !outer {
				t.Fatalf("protectionPlan outer = %v, err = %v; want the launch wrapped", outer, err)
			}
		})
	}

	// What does not count as caller environment: the operator's catalog launch
	// env, and the MCP allow-list a boot profile sets, which Tether consumes.
	plan := codexPlan()
	plan.Env = map[string]string{"GH_TOKEN": "operator-set-in-the-catalog"}
	applyMCPAllowlist(plan, bootgen.Profile{MCPServers: []string{"torque"}})
	if len(plan.CallerEnv) != 0 {
		t.Fatalf("operator env or the MCP allow-list was recorded as caller env: %v", plan.CallerEnv)
	}
	opts := codexStartOpts(t)
	if ok, why := codexOwnsSandbox(plan, &opts, []string{catalog}); !ok {
		t.Fatalf("a launch with only operator env was refused: %s", why)
	}

	// A relative TMPDIR in the DAEMON's environment is refused too: codex
	// resolves it against its own cwd, which no path check here can name.
	opts = codexStartOpts(t)
	opts.Env = append(opts.Env, "TMPDIR=../../catalog")
	if ok, why := codexOwnsSandbox(codexPlan(), &opts, []string{catalog}); ok || !strings.Contains(why, "TMPDIR is relative") {
		t.Fatalf("relative daemon TMPDIR: %v, %q", ok, why)
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// A TMPDIR that is an absolute ANCESTOR of CODEX_HOME: nothing overlaps a
// protected directory, but turn 1 can rewrite $CODEX_HOME/config.toml with a
// writable_roots, and turn 2 honors it. The real writable roots are in play
// here, not the cleared ones.
func TestCodexOwnsSandbox_TmpdirAboveCodexHome(t *testing.T) {
	_, catalog, _ := tetherLayout(t)
	opts := codexStartOpts(t)
	opts.Env = append(opts.Env, "TMPDIR="+filepath.Dir(opts.WorkspaceDir)) // an ancestor of the boot dir
	ok, why := codexOwnsSandbox(codexPlan(), &opts, []string{catalog})
	if ok || !strings.Contains(why, "CODEX_HOME") {
		t.Fatalf("codexOwnsSandbox = %v, %q; want CODEX_HOME inside the TMPDIR codex lets the agent write refused", ok, why)
	}
}

// Through LaunchSession: the same launches are wrapped, and where Tether's
// sandbox cannot start (this host) they fail loudly and never run the agent.
func TestLaunchSession_CodexWithCallerEnvIsWrappedNotExempted(t *testing.T) {
	svc, _, _ := tetherLayout(t)
	clearWritableRoots(t)
	bwrapAvailable = func(string) error { return errors.New("bwrap: No permissions to create a new namespace") }
	counter := filepath.Join(t.TempDir(), "runs")
	script := fmt.Sprintf("#!/bin/sh\necho run >> %q\n", counter)

	for name, mod := range map[string]func(*launch.Plan){
		"PATH from the caller": func(p *launch.Plan) {
			p.ProviderID = "codex-cli"
			applyProviderOverrides(p, map[string]config.ProviderOverride{"codex-cli": {Env: map[string]string{"PATH": "/tmp/fake-bin:/usr/bin"}}})
		},
		"a relative TMPDIR from the caller": func(p *launch.Plan) {
			if _, err := applyOverride(p, "x", `{"env":{"TMPDIR":"../../catalog"}}`); err != nil {
				t.Fatal(err)
			}
		},
		"LD_PRELOAD from the caller": func(p *launch.Plan) {
			p.ProviderID = "codex-cli"
			applyProviderOverrides(p, map[string]config.ProviderOverride{"codex-cli": {Env: map[string]string{"LD_PRELOAD": "/tmp/x.so"}}})
		},
		"nanite on the planted MCP allow-list": func(p *launch.Plan) {
			p.Env = map[string]string{launch.MCPServersEnv: "torque,nanite"}
		},
		"loom on the planted MCP allow-list: not known safe either": func(p *launch.Plan) {
			p.Env = map[string]string{launch.MCPServersEnv: "torque,loom"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := launchFakeCodex(t, svc, script, mod)
			if !errors.Is(err, launch.ErrProtectionUnavailable) {
				t.Fatalf("LaunchSession = %v; want the wrapped launch refused where bwrap cannot start", err)
			}
			if _, statErr := os.Stat(counter); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatal("the agent ran although its launch was refused")
			}
		})
	}

	// Negative control: with none of that the same launch is left to codex.
	sessID, _ := startFakeCodex(t, svc, script)
	if _, ok := svc.codexExempt.Load(sessID); !ok {
		t.Fatal("a plain codex launch was not left to its own sandbox, so the cases above prove nothing")
	}
}

// Every MCP child of an exempted codex agent runs outside its sandbox, because
// codex spawns MCP servers itself. Only the default allow-list's upstreams
// (torque, tesseract) are known to have no host-exec or arbitrary file-write
// tool, so a planted list that is a subset of them keeps the exemption and
// anything else wraps the agent, which fails loudly where Tether's sandbox
// cannot start: nanite (dev_bash, dev_write), cerberus, and equally loom,
// hadron, sigil, fragments-engine, tangent and any upstream added later.
// CW-20261001-0230 is the real fix.
func TestCodexOwnsSandbox_OnlyKnownSafeUpstreamsKeepTheExemption(t *testing.T) {
	svc, catalog, _ := tetherLayout(t)
	clearWritableRoots(t)
	for _, tc := range []struct {
		list string
		bad  string // the upstream named in the refusal; "" keeps the exemption
	}{
		{"", ""}, // the default: torque, tesseract
		{"torque,tesseract", ""},
		{"tesseract", ""},
		{"torque", ""},
		{" Torque , TESSERACT ", ""},
		{"torque,nanite", "nanite"},
		{"nanite", "nanite"},
		{"Cerberus", "Cerberus"},
		{"loom,hadron", "loom"},
		{"torque,tesseract,tangent", "tangent"}, // the live tangent-codex-app-server-worktree launch
		{"sigil", "sigil"},
		{"fragments-engine", "fragments-engine"},
		{"some-upstream-added-later", "some-upstream-added-later"},
	} {
		t.Run("allow-list "+tc.list, func(t *testing.T) {
			plan := codexPlan()
			if tc.list != "" {
				plan.Env = map[string]string{launch.MCPServersEnv: tc.list}
			}
			opts := codexStartOpts(t)
			ok, why := codexOwnsSandbox(plan, &opts, []string{catalog})
			if tc.bad == "" && !ok {
				t.Fatalf("refused: %s", why)
			}
			if tc.bad != "" && (ok || !strings.Contains(why, tc.bad) || !strings.Contains(why, "outside its sandbox") || !strings.Contains(why, "torque and tesseract")) {
				t.Fatalf("codexOwnsSandbox = %v, %q; want the agent wrapped, naming %s and what is allowed", ok, why, tc.bad)
			}
			if _, outer, err := svc.protectionPlan(plan, "cli", &opts, false); err != nil || outer != (tc.bad != "") {
				t.Fatalf("outer = %v, err = %v", outer, err)
			}
		})
	}
	// The boot profile's list reaches the plan through the real function.
	plan := codexPlan()
	applyMCPAllowlist(plan, bootgen.Profile{MCPServers: []string{"torque", "loom"}})
	if got := unsafeMCPUpstream(plan); got != "loom" {
		t.Fatalf("unsafeMCPUpstream after a boot profile naming loom = %q", got)
	}
	// The safe set is a constant of its own, not the default list, so changing
	// the default cannot silently widen what runs outside a sandbox. The default
	// must stay inside it, or every default codex launch would be wrapped.
	for _, id := range launch.DefaultMCPServers {
		found := false
		for _, safe := range codexSafeMCPUpstreams {
			found = found || strings.EqualFold(id, safe)
		}
		if !found {
			t.Fatalf("the default MCP allow-list names %s, which is not in codexSafeMCPUpstreams", id)
		}
	}
}

// The reviewer's second per-turn case: a project root that is a symlink,
// retargeted between turns. The launch judged where it pointed then; each turn
// resolves it afresh, and refuses one that now holds a project .codex/config.toml
// or contains the catalog.
func TestCodexExemptSession_RetargetedProjectRootIsRefused(t *testing.T) {
	svc, catalog, _ := tetherLayout(t)
	clearWritableRoots(t)
	base := t.TempDir()
	a, b := filepath.Join(base, "a"), filepath.Join(base, "b")
	for _, d := range []string{a, b} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	writeFileT(t, filepath.Join(b, ".codex", "config.toml"), "[sandbox_workspace_write]\nwritable_roots=[\"/c\"]\n")
	link := filepath.Join(base, "repo")
	if err := os.Symlink(a, link); err != nil {
		t.Fatal(err)
	}
	retarget := func(to string) {
		t.Helper()
		if err := os.Remove(link); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(to, link); err != nil {
			t.Fatal(err)
		}
	}
	counter := filepath.Join(t.TempDir(), "runs")
	script := fmt.Sprintf(`#!/bin/sh
echo run >> %q
echo '{"type":"item.completed","item":{"type":"agent_message","text":"RAN"}}'
echo '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}'
`, counter)
	sessID, _ := startFakeCodexWith(t, svc, script, func(p *launch.Plan) { p.RepoRoot = link })
	if _, ok := svc.codexExempt.Load(sessID); !ok {
		t.Fatal("the session was not left to codex's own sandbox")
	}
	runs := func() int {
		data, _ := os.ReadFile(counter) //nolint:gosec // test-owned path
		return strings.Count(string(data), "run")
	}
	waitRuns := func(n int) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for runs() < n && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		if got := runs(); got != n {
			t.Fatalf("the agent ran %d times, want %d", got, n)
		}
	}

	if err := svc.SendTurn(context.Background(), sessID, "one"); err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	waitRuns(1)

	retarget(b) // now holds a project .codex/config.toml
	if err := svc.SendTurn(context.Background(), sessID, "two"); !errors.Is(err, launch.ErrCodexSandboxWidened) || !strings.Contains(err.Error(), filepath.Join(b, ".codex", "config.toml")) {
		t.Fatalf("retargeted to a dir with a project config: err = %v; want it refused, naming the file", err)
	}
	retarget(filepath.Dir(catalog)) // now contains the catalog
	if err := svc.SendTurn(context.Background(), sessID, "three"); !errors.Is(err, launch.ErrCodexSandboxWidened) || !strings.Contains(err.Error(), "protected directory") {
		t.Fatalf("retargeted to a dir containing the catalog: err = %v; want it refused", err)
	}
	time.Sleep(300 * time.Millisecond)
	if got := runs(); got != 1 {
		t.Fatalf("a refused turn ran the agent: %d runs, want 1", got)
	}

	retarget(a) // back where it was: the turn runs again
	if err := svc.SendTurn(context.Background(), sessID, "four"); err != nil {
		t.Fatalf("turn after retargeting back: %v", err)
	}
	waitRuns(2)
}

// The planted `mux mcp` is told what to refuse writing, from the same decision
// that registers the agent's ProtectedPaths, so a runtime that spawns it outside
// Tether's sandbox gets the same refusal (CW-20261001-0142 blocker 1: real codex
// called mux_agent_create scope=system and wrote the catalog).
func TestMuxMCPPlant_ProtectPathArgs(t *testing.T) {
	args := MuxMCPPlant("/catalog", "sess-1", false, "/c/catalog", "/c/run").Args
	got := ""
	for i, a := range args {
		if a == "--protect-path" && i+1 < len(args) {
			got += args[i+1] + ";"
		}
	}
	if got != "/c/catalog;/c/run;" {
		t.Fatalf("argv %v: --protect-path values = %q", args, got)
	}
	// The operator's own `mux boot` (no session) and an unprotected launch get none.
	if args := MuxMCPPlant("/catalog", "", false, "/c/catalog").Args; contains(args, "--protect-path") {
		t.Fatalf("a sessionless plant carries --protect-path: %v", args)
	}
	if args := MuxMCPPlant("/catalog", "sess-1", false).Args; contains(args, "--protect-path") {
		t.Fatalf("a launch with nothing protected carries --protect-path: %v", args)
	}
}

// Through LaunchSession, the planted codex config.toml carries --protect-path
// for the catalog root, the run directory and the state directory while
// protection is on, and none while it is off.
func TestLaunchSession_PlantsProtectPathsInTheMCPConfig(t *testing.T) {
	plantedArgs := func(t *testing.T, svc *Service, wantProtection bool) string {
		t.Helper()
		_, ws := startFakeCodex(t, svc, "#!/bin/sh\n")
		matches, err := filepath.Glob(filepath.Join(ws.Root, "boot", "agentlaunch-bootdir-*", "config.toml"))
		if err != nil || len(matches) != 1 {
			t.Fatalf("planted config.toml: %v, %v", matches, err)
		}
		data, err := os.ReadFile(matches[0]) //nolint:gosec // test-owned path
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}

	svc, catalog, run := tetherLayout(t)
	state := filepath.Join(filepath.Dir(catalog), "state")
	clearWritableRoots(t)
	on := plantedArgs(t, svc, true)
	if !strings.Contains(on, `"TETHER_MCP_CONFINE_REMOTE" = "1"`) || !strings.Contains(on, `"--ro-bind"`) {
		t.Fatalf("Codex proxy lacks local wrapper or remote confinement: %s", on)
	}
	for _, want := range []string{`"--protect-path", "` + catalog + `"`, `"--protect-path", "` + run + `"`, `"--protect-path", "` + state + `"`} {
		if !strings.Contains(on, want) {
			t.Errorf("planted config.toml lacks %s:\n%s", want, on)
		}
	}

	// A separate service: the helper replaces a Service's manager and store.
	offSvc, _, _ := tetherLayout(t)
	offSvc.protectionStatus = func() ProtectionStatus { return ProtectionStatus{} } // protection off
	if off := plantedArgs(t, offSvc, false); strings.Contains(off, "--protect-path") {
		t.Errorf("an unprotected launch was planted with --protect-path:\n%s", off)
	}
}
