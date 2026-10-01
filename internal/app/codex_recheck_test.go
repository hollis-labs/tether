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

	"github.com/hollis-labs/tether/internal/launch"
)

// plantedConfig is the shape of the config.toml Tether plants into CODEX_HOME,
// and codexTrust is the entry codex adds to it itself.
const plantedConfig = `approval_policy = "on-request"
sandbox_mode = "workspace-write"

[mcp_servers.mux]
command = "/opt/mux"
args = ["--catalog", "/c", "mcp", "--proxy", "--session", "s1"]

[mcp_servers.mux.env]
MUX_MCP_SERVERS = "a,b"
`

const codexTrust = `
[projects."/work/proj"]
trust_level = "trusted"
`

// codexConfigUnsafe is a strict allowlist over the config.toml codex reads from
// CODEX_HOME: what Tether plants and what codex adds is safe, a widened sandbox
// is not, and anything unrecognized is not. It never echoes a value.
func TestCodexConfigUnsafe(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		bad  string // "" = safe; otherwise a substring of the reason
	}{
		{"planted shape", plantedConfig, ""},
		{"planted shape plus codex's trust entry", plantedConfig + codexTrust, ""},
		{"CRLF, comments and blanks", "# planted\r\n\r\napproval_policy = \"never\" # asks nothing\r\nsandbox_mode = \"read-only\"\r\n", ""},
		{"empty file", "", ""},
		{"an MCP server with a tool table", plantedConfig + "\n[mcp_servers.mux.tools.x]\napproval_mode = \"auto\"\n", ""},

		{"[sandbox_workspace_write] table", plantedConfig + "\n[sandbox_workspace_write]\nwritable_roots = [\"/cat\"]\n", "opens the table [sandbox_workspace_write"},
		{"table with spaces", plantedConfig + "\n[ sandbox_workspace_write ]\nwritable_roots = [\"/cat\"]\n", "opens the table"},
		{"top-level dotted key", plantedConfig[:strings.Index(plantedConfig, "\n[")] + "\nsandbox_workspace_write.writable_roots = [\"/cat\"]\n", "sandbox_workspace_write.writable_roots"},
		{"inline table", "sandbox_workspace_write = { writable_roots = [\"/cat\"] }\n" + plantedConfig, "sandbox_workspace_write"},
		{"danger-full-access", "sandbox_mode = \"danger-full-access\"\n", "switches codex's sandbox off"},
		{"quoted key", "\"sandbox_mode\" = \"workspace-write\"\n", "not a key Tether plants"},
		{"unknown approval value", "approval_policy = \"yolo\"\n", "approval_policy to a value Tether does not know"},
		{"profile table", plantedConfig + "\n[profiles.p]\nsandbox_mode = \"danger-full-access\"\n", "opens the table [profiles.p"},
		{"array of tables", plantedConfig + "\n[[x]]\n", "opens the table"},
		{"permissions table", plantedConfig + "\n[permissions.fs]\nread = []\n", "opens the table [permissions.fs"},
		{"default_permissions", "default_permissions = \"open\"\n" + plantedConfig, "default_permissions"},
		{"unknown top-level key", "model = \"gpt-5\"\n" + plantedConfig, "model"},
		{"project table with another key", plantedConfig + "\n[projects.\"/p\"]\nwritable_roots = [\"/cat\"]\n", "in a project table"},
		{"bare mcp_servers table", plantedConfig + "\n[mcp_servers]\nx = 1\n", "opens the table [mcp_servers"},
		{"top-level key after a table cannot hide", plantedConfig + "\n[projects.\"/p\"]\ntrust_level = \"trusted\"\nsandbox_mode = \"danger-full-access\"\n", "in a project table"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			got := codexConfigUnsafe(path)
			if tc.bad == "" && got != "" {
				t.Fatalf("codexConfigUnsafe = %q; want it known-safe", got)
			}
			if tc.bad != "" && !strings.Contains(got, tc.bad) {
				t.Fatalf("codexConfigUnsafe = %q; want a reason containing %q", got, tc.bad)
			}
		})
	}

	// An absent config is nothing to widen; an unreadable one is not known-safe.
	if got := codexConfigUnsafe(filepath.Join(t.TempDir(), "absent.toml")); got != "" {
		t.Errorf("absent config: %q", got)
	}
	if got := codexConfigUnsafe(t.TempDir()); !strings.Contains(got, "cannot be read") {
		t.Errorf("a directory where the config should be: %q", got)
	}

	// A reason names a line and a key, never a value: a value may be a secret.
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("api_key = \"sk-secret-value-1234\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := codexConfigUnsafe(path); got == "" || strings.Contains(got, "sk-secret") {
		t.Errorf("reason = %q; want a refusal that does not echo the value", got)
	}
}

// A session left to codex's own sandbox is refused its next turn when a project
// .codex/config.toml appears in its work directory, or its CODEX_HOME
// config.toml stops having the shape Tether planted. Codex keeps .codex
// read-only only from CODEX, so another agent sharing the directory, a git
// checkout or an operator can plant it between turns (CW-20261001-0142).
func TestRefuseWidenedCodex(t *testing.T) {
	svc, _, _ := tetherLayout(t)
	work, home := t.TempDir(), t.TempDir()
	cfg := filepath.Join(home, "config.toml")
	if err := os.WriteFile(cfg, []byte(plantedConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	// A session that was never exempt is not in the registry, and passes.
	if err := svc.refuseWidenedCodex("not-exempt"); err != nil {
		t.Fatalf("unregistered session: %v", err)
	}
	svc.codexExempt.Store("s1", &codexExemption{workDirs: []string{work}, home: home})

	// Negative control: nothing planted, the turn goes ahead, and codex adding
	// its own trust entry does not count.
	if err := svc.refuseWidenedCodex("s1"); err != nil {
		t.Fatalf("clean session refused: %v", err)
	}
	if err := os.WriteFile(cfg, []byte(plantedConfig+codexTrust), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := svc.refuseWidenedCodex("s1"); err != nil {
		t.Fatalf("codex's own trust entry refused a turn: %v", err)
	}

	// A project config appears.
	proj := filepath.Join(work, ".codex", "config.toml")
	writeFileT(t, proj, "[sandbox_workspace_write]\nwritable_roots=[\"/c\"]\n")
	err := svc.refuseWidenedCodex("s1")
	if !errors.Is(err, launch.ErrCodexSandboxWidened) || !strings.Contains(err.Error(), proj) ||
		!strings.Contains(err.Error(), "remove it, or relaunch") || strings.Contains(err.Error(), "writable_roots") {
		t.Fatalf("err = %v; want ErrCodexSandboxWidened naming the file and the recovery, never its contents", err)
	}
	// Removing it is the recovery.
	if err := os.Remove(proj); err != nil {
		t.Fatal(err)
	}
	if err := svc.refuseWidenedCodex("s1"); err != nil {
		t.Fatalf("after removing the file: %v", err)
	}

	// CODEX_HOME's config.toml is tampered with.
	if err := os.WriteFile(cfg, []byte(plantedConfig+"\n[sandbox_workspace_write]\nwritable_roots=[\"/c\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := svc.refuseWidenedCodex("s1"); !errors.Is(err, launch.ErrCodexSandboxWidened) || !strings.Contains(err.Error(), cfg) {
		t.Fatalf("tampered CODEX_HOME config: err = %v", err)
	}
}

// Through the real path, SendTurn and SendInput on a stand-in codex session: a
// turn runs, a project .codex/config.toml is planted from outside, the next turn
// is refused and never runs (the wake sweep, the app-server turn/start and the
// HTTP and MCP routes all come through these two), and it runs again once the
// file is gone. The entry is dropped when the session ends.
func TestCodexExemptSession_TurnRefusedWhenProjectConfigAppears(t *testing.T) {
	svc, _, _ := tetherLayout(t)
	clearWritableRoots(t)
	counter := filepath.Join(t.TempDir(), "runs")
	script := fmt.Sprintf(`#!/bin/sh
echo run >> %q
echo '{"type":"item.completed","item":{"type":"agent_message","text":"RAN"}}'
echo '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}'
`, counter)
	sessID, _ := startFakeCodex(t, svc, script)
	if _, ok := svc.codexExempt.Load(sessID); !ok {
		t.Fatal("the stand-in codex session was not left to codex's own sandbox, so there is nothing to re-check")
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

	plan, err := svc.Store.GetLaunchPlan(sessID)
	if err != nil {
		t.Fatal(err)
	}
	planted := filepath.Join(plan.RepoRoot, ".codex", "config.toml")
	writeFileT(t, planted, "[sandbox_workspace_write]\nwritable_roots=[\"/c\"]\n")

	for name, send := range map[string]func() error{
		"SendTurn":  func() error { return svc.SendTurn(context.Background(), sessID, "two") },
		"SendInput": func() error { return svc.SendInput(sessID, []byte("two")) },
	} {
		if err := send(); !errors.Is(err, launch.ErrCodexSandboxWidened) || !strings.Contains(err.Error(), planted) {
			t.Fatalf("%s with %s planted = %v; want ErrCodexSandboxWidened naming it", name, planted, err)
		}
	}
	time.Sleep(300 * time.Millisecond)
	if got := runs(); got != 1 {
		t.Fatalf("a refused turn still ran the agent: %d runs, want 1", got)
	}

	// Recovery: the file is removed, and the next turn runs.
	if err := os.Remove(planted); err != nil {
		t.Fatal(err)
	}
	if err := svc.SendTurn(context.Background(), sessID, "three"); err != nil {
		t.Fatalf("turn after removing the file: %v", err)
	}
	waitRuns(2)

	// The entry goes when the session does.
	if err := svc.Manager.Stop(context.Background(), sessID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, ok := svc.codexExempt.Load(sessID); !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the exemption entry outlived its session")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Negative control for the above: with nothing planted a second turn runs, so
// the refusal is the check and not some other effect of the setup.
func TestCodexExemptSession_SecondTurnRunsWhenNothingIsPlanted(t *testing.T) {
	svc, _, _ := tetherLayout(t)
	clearWritableRoots(t)
	counter := filepath.Join(t.TempDir(), "runs")
	script := fmt.Sprintf(`#!/bin/sh
echo run >> %q
echo '{"type":"item.completed","item":{"type":"agent_message","text":"RAN"}}'
echo '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}'
`, counter)
	sessID, _ := startFakeCodex(t, svc, script)
	for _, text := range []string{"one", "two"} {
		if err := svc.SendTurn(context.Background(), sessID, text); err != nil {
			t.Fatalf("turn %q: %v", text, err)
		}
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		data, _ := os.ReadFile(counter) //nolint:gosec // test-owned path
		if strings.Count(string(data), "run") >= 2 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the second turn never ran (%q)", data)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// At launch, CODEX_HOME inside a directory codex's sandbox lets the agent write
// is not left to codex: the agent could plant its own config.toml there in one
// turn and have the next read it. And a CODEX_HOME config.toml that is already
// not of the planted shape is not left to codex either.
func TestCodexOwnsSandbox_CodexHomeMustNotBeWritable(t *testing.T) {
	svc, catalog, _ := tetherLayout(t)
	clearWritableRoots(t)

	// CODEX_HOME under the project's work root.
	opts := codexStartOpts(t)
	plan := codexPlan()
	plan.WorkRoot = opts.WorkspaceDir // the boot dir lies under the workspace
	if ok, why := codexOwnsSandbox(plan, &opts, []string{catalog}); ok || !strings.Contains(why, "CODEX_HOME") {
		t.Fatalf("codexOwnsSandbox = %v, %q; want CODEX_HOME inside the work root refused", ok, why)
	}
	// And under /tmp or $TMPDIR, which the sandbox always lets the agent write.
	opts = codexStartOpts(t)
	prev := codexExtraWritableRoots
	codexExtraWritableRoots = func([]string) []string { return []string{opts.WorkspaceDir} }
	t.Cleanup(func() { codexExtraWritableRoots = prev })
	if ok, why := codexOwnsSandbox(codexPlan(), &opts, []string{catalog}); ok || !strings.Contains(why, "CODEX_HOME") {
		t.Fatalf("codexOwnsSandbox = %v, %q; want CODEX_HOME inside a temp dir refused", ok, why)
	}
	codexExtraWritableRoots = prev
	clearWritableRoots(t)

	// A config.toml of the planted shape is fine; a widened one is not.
	opts = codexStartOpts(t)
	cfg := filepath.Join(codexHomeFromEnv(opts.Env), "config.toml")
	writeFileT(t, cfg, plantedConfig+codexTrust)
	if ok, why := codexOwnsSandbox(codexPlan(), &opts, []string{catalog}); !ok {
		t.Fatalf("planted-shape config refused: %s", why)
	}
	writeFileT(t, cfg, plantedConfig+"\n[sandbox_workspace_write]\nwritable_roots=[\""+catalog+"\"]\n")
	if ok, why := codexOwnsSandbox(codexPlan(), &opts, []string{catalog}); ok || !strings.Contains(why, "sandbox_workspace_write") {
		t.Fatalf("widened CODEX_HOME config: %v, %q", ok, why)
	}
	// Wrapped, then, by the real decision.
	if _, outer, err := svc.protectionPlan(codexPlan(), "cli", &opts, false); err != nil || !outer {
		t.Fatalf("protectionPlan outer = %v, err = %v; want the launch wrapped", outer, err)
	}
}
