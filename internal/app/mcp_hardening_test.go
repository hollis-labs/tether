package app

// CW-20261001-0227: a Tether-launched Claude loads only the MCP servers
// Tether plants.

import (
	"context"
	"slices"
	"testing"

	gop "github.com/hollis-labs/substrate/harness/adapters/provider"
	"github.com/hollis-labs/substrate/harness/agentlaunch"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launch"
)

func TestClaudeStrictMCP_Env(t *testing.T) {
	for _, tc := range []struct {
		val          string
		wantEnabled  bool
		wantDisabled bool
	}{
		{"", true, false},
		{"1", true, false},
		{"true", true, false},
		{"anything-else", true, false},
		{"0", false, true},
		{"false", false, true},
		{" FALSE ", false, true},
	} {
		st := ClaudeStrictMCP(func(string) string { return tc.val })
		if st.Enabled != tc.wantEnabled || st.DisabledByOperator != tc.wantDisabled || st.Reason == "" {
			t.Fatalf("%s=%q: %+v; want enabled=%v disabledByOperator=%v with a reason", ClaudeStrictMCPEnv, tc.val, st, tc.wantEnabled, tc.wantDisabled)
		}
	}
}

func strictService(enabled bool) *Service {
	return &Service{strictMCPStatus: func() StrictMCPStatus {
		if enabled {
			return StrictMCPStatus{Enabled: true}
		}
		return StrictMCPStatus{DisabledByOperator: true}
	}}
}

func TestApplyClaudeStrictMCP(t *testing.T) {
	flag := "--strict-mcp-config"
	mk := func(id string, flags ...string) agentlaunch.LaunchPlan {
		return agentlaunch.LaunchPlan{Provider: agentlaunch.ProviderSpec{ID: id, Flags: flags}}
	}

	// On: claude gets it once, after the catalog's own flags.
	lp := mk("claude", "--model", "x")
	orig := lp.Provider.Flags
	strictService(true).applyClaudeStrictMCP(&lp)
	if !slices.Equal(lp.Provider.Flags, []string{"--model", "x", flag}) {
		t.Fatalf("flags = %q", lp.Provider.Flags)
	}
	if len(orig) != 2 {
		t.Fatal("the caller's flag slice was mutated")
	}

	// Already in the catalog's args: not repeated.
	lp = mk("claude", flag)
	strictService(true).applyClaudeStrictMCP(&lp)
	if n := countToken(lp.Provider.Flags, flag); n != 1 {
		t.Fatalf("%q appears %d times: %q", flag, n, lp.Provider.Flags)
	}

	// Applying twice (the plan is built more than once per session) still
	// leaves one.
	lp = mk("claude")
	svc := strictService(true)
	svc.applyClaudeStrictMCP(&lp)
	svc.applyClaudeStrictMCP(&lp)
	if n := countToken(lp.Provider.Flags, flag); n != 1 {
		t.Fatalf("%q appears %d times after two applications", flag, n)
	}

	// Off (the kill switch), and every other provider: untouched.
	for name, c := range map[string]struct {
		svc *Service
		id  string
	}{
		"kill switch": {strictService(false), "claude"},
		"codex":       {strictService(true), "codex"},
		"opencode":    {strictService(true), "opencode"},
		"antigravity": {strictService(true), "antigravity"},
	} {
		lp := mk(c.id, "--keep")
		c.svc.applyClaudeStrictMCP(&lp)
		if !slices.Equal(lp.Provider.Flags, []string{"--keep"}) {
			t.Fatalf("%s: flags = %q; want them unchanged", name, lp.Provider.Flags)
		}
	}
}

// The flag is in the argv of every turn the template resolves, not just the
// first, and not after the "--" that ends options, so a later turn or the
// resume path is as strict as the boot turn.
func TestClaudeStrictMCP_EveryTurnOfTheLaunchTemplate(t *testing.T) {
	for _, tc := range []struct {
		name        string
		runtimeKind string
	}{
		{"streaming-stdio", config.RuntimeKindStreamingStdio},
		{"subprocess", config.RuntimeKindSubprocess},
		{"pty", config.RuntimeKindPTY},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := strictService(true)
			svc.CatalogRoot = t.TempDir()
			svc.Catalog = &config.Catalog{Global: config.Global{Version: "test"}}
			ws, repo := t.TempDir(), t.TempDir()
			plan := &launch.Plan{
				LaunchID: "demo", ProjectID: "project", LogicalAgentID: "agent",
				ProviderID: "claude-code", ProviderBrand: "claude", RuntimeKind: tc.runtimeKind,
				RepoRoot: repo, WriteHome: ws, WorkspaceMode: "shared", Command: "claude",
				BootPrompt: testBootPrompt,
			}
			prepared, err := svc.prepareSharedLaunch(context.Background(), plan, ws, plantContextInput{TetherCommand: "tether", TetherArgs: []string{"mcp"}})
			if err != nil {
				t.Fatal(err)
			}
			for turn, in := range []gop.TurnInput{{Prompt: "first"}, {Prompt: "second", ResumeID: "sess-1"}} {
				argv, err := prepared.Launch.TurnArgv(in)
				if err != nil {
					t.Fatalf("turn %d: %v", turn, err)
				}
				if n := countToken(argv, "--strict-mcp-config"); n != 1 {
					t.Fatalf("turn %d: --strict-mcp-config appears %d times: %q", turn, n, argv)
				}
				// Exactly one MCP config, the planted one: strict mode is only
				// meaningful if --mcp-config still names it.
				if n := countToken(argv, "--mcp-config"); n != 1 {
					t.Fatalf("turn %d: --mcp-config appears %d times: %q", turn, n, argv)
				}
				if end := slices.Index(argv, "--"); end >= 0 && slices.Index(argv, "--strict-mcp-config") > end {
					t.Fatalf("turn %d: --strict-mcp-config is after the end-of-options marker: %q", turn, argv)
				}
			}
		})
	}
}

func TestClaudeStrictMCP_KillSwitchLeavesTheArgvAlone(t *testing.T) {
	svc := strictService(false)
	svc.CatalogRoot = t.TempDir()
	svc.Catalog = &config.Catalog{Global: config.Global{Version: "test"}}
	ws, repo := t.TempDir(), t.TempDir()
	plan := &launch.Plan{
		LaunchID: "demo", ProjectID: "project", LogicalAgentID: "agent",
		ProviderID: "claude-code", ProviderBrand: "claude", RuntimeKind: config.RuntimeKindStreamingStdio,
		RepoRoot: repo, WriteHome: ws, WorkspaceMode: "shared", Command: "claude", BootPrompt: testBootPrompt,
	}
	prepared, err := svc.prepareSharedLaunch(context.Background(), plan, ws, plantContextInput{TetherCommand: "tether", TetherArgs: []string{"mcp"}})
	if err != nil {
		t.Fatal(err)
	}
	argv, err := prepared.Launch.TurnArgv(gop.TurnInput{Prompt: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(argv, "--strict-mcp-config") {
		t.Fatalf("--strict-mcp-config in the argv with the kill switch off: %q", argv)
	}
}
