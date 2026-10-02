package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/agentkit/agentsessions"
	gop "github.com/hollis-labs/go-providers/provider"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/workspace"
)

const testBootPrompt = "BOOT PROMPT: read the task and begin"

// One argv owner (CW-20260930-0135, agentkit v0.13.0): a launched session's
// every turn spawns the shared launch's template resolved for that turn
// (StartOptions.Launch), not the runtime adapter's BuildArgs. For each
// runtime the catalog launches, that argv names each flag once, keeps the
// boot prompt out, carries the args only the launch knows (the planted
// --mcp-config, the project dir) and puts the turn last after "--". Tether's
// interim sharedExtraArgs and its splice are gone (CW-20261001-0095).
func TestLaunchTemplate_ComposesArgvOnce(t *testing.T) {
	const turnPrompt = "--dangerously-bypass-everything"
	for _, tc := range []struct {
		name        string
		providerID  string
		brand       string
		runtimeKind string
		mode        string // Tether permission mode
		args        []string
		wantPairs   [][2]string // flag, root ("boot" | "project")
		wantOnce    []string
		wantValues  [][2]string // flag, the value that follows it
		wantAbsent  []string
		wantNoEnv   []string
		wantPrompt  string // "" when the turn goes over stdin, not argv
	}{
		{
			name:        "claude streaming-stdio",
			providerID:  "claude-code",
			brand:       "claude",
			runtimeKind: config.RuntimeKindStreamingStdio,
			mode:        config.PermissionModeBypass,
			wantPairs:   [][2]string{{"--mcp-config", "boot"}, {"--add-dir", "project"}},
			wantOnce:    []string{"-p", "--input-format", "--output-format", "--mcp-config", "--permission-mode"},
			// The posture owns the permission flag (CW-20261001-0156).
			wantValues: [][2]string{{"--permission-mode", "bypassPermissions"}},
			wantAbsent: []string{"--dangerously-skip-permissions"},
		},
		{
			name:        "claude print turn",
			providerID:  "claude-sub",
			brand:       "claude",
			runtimeKind: config.RuntimeKindSubprocess,
			mode:        config.PermissionModeDefault,
			wantPairs:   [][2]string{{"--mcp-config", "boot"}, {"--add-dir", "project"}},
			wantOnce:    []string{"-p", "--output-format", "--mcp-config", "--permission-mode"},
			wantValues:  [][2]string{{"--permission-mode", "default"}},
			wantAbsent:  []string{"--dangerously-skip-permissions"},
			wantPrompt:  turnPrompt,
		},
		{
			name:        "codex app-server",
			args:        []string{"app-server"},
			providerID:  "codex-app-server",
			brand:       "codex",
			runtimeKind: config.RuntimeKindJSONRPCStdio,
			mode:        config.PermissionModeBypass,
			// Explicit bypass disables provider sandboxing and approvals.
			wantOnce: []string{"app-server", `sandbox_mode="danger-full-access"`, `approval_policy="never"`},
		},
		{
			name:        "codex exec turn",
			providerID:  "codex-cli",
			brand:       "codex",
			runtimeKind: config.RuntimeKindSubprocess,
			mode:        config.PermissionModeDefault,
			wantPairs:   [][2]string{{"--cd", "project"}},
			wantOnce:    []string{"exec", "--json", "--skip-git-repo-check", "--cd", `sandbox_mode="workspace-write"`, `approval_policy="on-request"`},
			wantPrompt:  turnPrompt,
		},
		{
			name:        "codex exec bypass turn",
			providerID:  "codex-cli",
			brand:       "codex",
			runtimeKind: config.RuntimeKindSubprocess,
			mode:        config.PermissionModeBypass,
			wantPairs:   [][2]string{{"--cd", "project"}},
			wantOnce:    []string{"exec", "--json", `sandbox_mode="danger-full-access"`, `approval_policy="never"`},
			wantPrompt:  turnPrompt,
		},
		{
			// The live catalog's opencode: `args: [run]`, the planted agent.
			name:        "opencode run turn",
			providerID:  "opencode",
			brand:       "opencode",
			runtimeKind: config.RuntimeKindSubprocess,
			mode:        config.PermissionModeBypass,
			args:        []string{"run"},
			wantPairs:   [][2]string{{"--dir", "project"}},
			wantOnce:    []string{"run", "--format", "--agent", "tether-agent"},
			// No posture: opencode keeps its own defaults.
			wantNoEnv:  []string{"OPENCODE_PERMISSION"},
			wantPrompt: turnPrompt,
		},
		{
			// Catalog flags other than the leading run reach argv once, at
			// the convention's extra slot.
			name:        "opencode run with catalog --model",
			providerID:  "opencode",
			brand:       "opencode",
			runtimeKind: config.RuntimeKindSubprocess,
			args:        []string{"run", "--model", "anthropic/claude-x"},
			wantPairs:   [][2]string{{"--dir", "project"}},
			wantOnce:    []string{"run", "--model", "anthropic/claude-x", "--agent", "tether-agent"},
			wantPrompt:  turnPrompt,
		},
		{
			name:        "antigravity turn",
			providerID:  "agy",
			brand:       "antigravity",
			runtimeKind: config.RuntimeKindSubprocess,
			mode:        config.PermissionModeBypass,
			wantPairs:   [][2]string{{"--add-dir", "project"}},
			wantOnce:    []string{"--output-format", "-p=" + turnPrompt, "--dangerously-skip-permissions"},
		},
		{
			name:        "antigravity turn, default mode",
			providerID:  "agy",
			brand:       "antigravity",
			runtimeKind: config.RuntimeKindSubprocess,
			mode:        config.PermissionModeDefault,
			wantPairs:   [][2]string{{"--add-dir", "project"}},
			wantValues:  [][2]string{{"--mode", "accept-edits"}},
			wantAbsent:  []string{"--dangerously-skip-permissions"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := &Service{
				CatalogRoot: t.TempDir(),
				Catalog:     &config.Catalog{Global: config.Global{Version: "test"}},
				// Strict MCP on, whatever this process's environment says.
				strictMCPStatus: func() StrictMCPStatus { return ClaudeStrictMCP(func(string) string { return "" }) },
			}
			ws := t.TempDir()
			repo := t.TempDir()
			plan := &launch.Plan{
				LaunchID:       "demo",
				ProjectID:      "project",
				LogicalAgentID: "agent",
				ProviderID:     tc.providerID,
				ProviderBrand:  tc.brand,
				RuntimeKind:    tc.runtimeKind,
				RepoRoot:       repo,
				WriteHome:      ws,
				WorkspaceMode:  "shared",
				Command:        tc.brand,
				Args:           tc.args,
				PermissionMode: tc.mode,
				BootPrompt:     testBootPrompt,
			}
			prepared, err := svc.prepareSharedLaunch(context.Background(), plan, ws, plantContextInput{
				TetherCommand: "tether",
				TetherArgs:    []string{"mcp"},
			})
			if err != nil {
				t.Fatalf("prepareSharedLaunch: %v", err)
			}
			if prepared.Launch == nil {
				t.Fatal("prepared launch has no template; the session would fall back to the adapter's argv")
			}
			argv, err := prepared.Launch.TurnArgv(gop.TurnInput{Prompt: turnPrompt})
			if err != nil {
				t.Fatalf("TurnArgv: %v", err)
			}

			if slices.Contains(argv, testBootPrompt) {
				t.Errorf("boot prompt is in a turn's argv: %q", argv)
			}
			assertNoRepeatedFlags(t, argv)
			// Only Claude loads MCP servers beyond the one config it is
			// handed, so only Claude is made strict (CW-20261001-0227).
			if tc.brand == "claude" {
				if n := countToken(argv, "--strict-mcp-config"); n != 1 {
					t.Errorf("--strict-mcp-config appears %d times, want 1: %q", n, argv)
				}
			} else if slices.Contains(argv, "--strict-mcp-config") {
				t.Errorf("--strict-mcp-config in a %s argv: %q", tc.brand, argv)
			}
			for _, tok := range tc.wantOnce {
				if n := countToken(argv, tok); n != 1 {
					t.Errorf("%q appears %d times, want 1: %q", tok, n, argv)
				}
			}
			for _, pair := range tc.wantValues {
				if i := slices.Index(argv, pair[0]); i < 0 || i+1 >= len(argv) || argv[i+1] != pair[1] {
					t.Errorf("want %s %s: %q", pair[0], pair[1], argv)
				}
			}
			for _, tok := range tc.wantAbsent {
				if slices.Contains(argv, tok) {
					t.Errorf("%q in argv: %q", tok, argv)
				}
			}
			for _, name := range tc.wantNoEnv {
				if v, ok := prepared.Env[name]; ok {
					t.Errorf("%s=%q in the launch env", name, v)
				}
			}
			if tc.wantPrompt != "" {
				assertPromptLast(t, argv, tc.wantPrompt)
			}
			roots := map[string]string{"boot": prepared.PlantedBootDir, "project": repo}
			for _, pair := range tc.wantPairs {
				i := slices.Index(argv, pair[0])
				if i < 0 || i+1 >= len(argv) {
					t.Errorf("%s missing: %q", pair[0], argv)
					continue
				}
				if rel, err := filepath.Rel(roots[pair[1]], argv[i+1]); err != nil || strings.HasPrefix(rel, "..") {
					t.Errorf("%s %q is not under the %s root %q", pair[0], argv[i+1], pair[1], roots[pair[1]])
				}
			}
		})
	}
}

// opencode merges a planted agent file into its built-in agent of the same
// name, so the shared launch plants and selects a namespaced agent: the file
// is agents/tether-<agent>.md and the template's --agent names it.
func TestOpencodePlantedAgentIsNamespaced(t *testing.T) {
	svc := &Service{CatalogRoot: t.TempDir(), Catalog: &config.Catalog{Global: config.Global{Version: "test"}}}
	ws := t.TempDir()
	plan := &launch.Plan{
		LaunchID: "demo", ProjectID: "project", LogicalAgentID: "general",
		ProviderID: "opencode", ProviderBrand: "opencode", RuntimeKind: config.RuntimeKindSubprocess,
		RepoRoot: t.TempDir(), WriteHome: ws, WorkspaceMode: "shared", Command: "opencode", Args: []string{"run"}, BootPrompt: testBootPrompt,
	}
	if got := launch.OpencodeAgentName(plan); got != "tether-general" {
		t.Fatalf("OpencodeAgentName = %q, want tether-general", got)
	}
	prepared, err := svc.prepareSharedLaunch(context.Background(), plan, ws, plantContextInput{TetherCommand: "tether"})
	if err != nil {
		t.Fatalf("prepareSharedLaunch: %v", err)
	}
	var planted []string
	_ = filepath.WalkDir(prepared.PlantedBootDir, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && filepath.Base(path) == "tether-general.md" {
			planted = append(planted, path)
		}
		return nil
	})
	if len(planted) == 0 {
		t.Fatalf("no agents/tether-general.md planted under %s", prepared.PlantedBootDir)
	}
	argv, err := prepared.Launch.TurnArgv(gop.TurnInput{Prompt: "go"})
	if err != nil {
		t.Fatal(err)
	}
	if i := slices.Index(argv, "--agent"); i < 0 || argv[i+1] != "tether-general" {
		t.Fatalf("turn argv does not select the namespaced agent: %q", argv)
	}
	if lp := launch.AgentLaunchPlan(&launch.Plan{ProviderBrand: "claude", LogicalAgentID: "general"}, ws); lp.Agent.Name != "" {
		t.Fatalf("claude AgentSpec.Name = %q, want empty (only opencode is namespaced)", lp.Agent.Name)
	}
}

// End to end through LaunchSession and agentkit's streaming-stdio runtime: a
// stand-in claude records the argv it was spawned with and the first line it
// reads from stdin.
func TestLaunchSession_StreamingStdioClaudeArgvAndBootTurn(t *testing.T) {
	dir := t.TempDir()
	argvFile := filepath.Join(dir, "argv")
	stdinFile := filepath.Join(dir, "stdin")
	fake := filepath.Join(dir, "claude")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argvFile + ".tmp && mv " + argvFile + ".tmp " + argvFile + "\n" +
		"head -n 1 > " + stdinFile + ".tmp && mv " + stdinFile + ".tmp " + stdinFile + "\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil { //nolint:gosec // test stand-in must be executable
		t.Fatal(err)
	}

	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	wsRoot := t.TempDir()
	repo := t.TempDir()
	const sessID = "sess-streaming-boot"
	plan := &launch.Plan{
		LaunchID:       "tether-claude",
		ProjectID:      "proj",
		LogicalAgentID: "agent",
		ProviderID:     "claude-code",
		ProviderBrand:  "claude",
		RuntimeKind:    config.RuntimeKindStreamingStdio,
		RepoRoot:       repo,
		WriteHome:      wsRoot,
		WorkspaceMode:  "shared",
		Command:        fake,
		PermissionMode: config.PermissionModeBypass,
		BootPrompt:     testBootPrompt,
		// What launch.Resolve copies from a claude-code provider's bootstrap.mode.
		BootMode: config.RuntimeKindStreamingStdio,
	}
	ws, err := workspace.Create(wsRoot, sessID, plan)
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
	svc := &Service{
		CatalogRoot: t.TempDir(),
		Catalog:     &config.Catalog{Global: config.Global{Version: "test"}},
		Store:       db,
		Manager:     agentsessions.NewManager(stateSinkAdapter{db: db}),
		factories:   map[string]RuntimeFactory{"claude-code": newClaudeStreamingStdioRuntime("claude-code")},
	}
	launched, err := svc.LaunchSession(sessID)
	if err != nil {
		t.Fatalf("LaunchSession: %v", err)
	}
	t.Cleanup(func() { _ = svc.Manager.Stop(context.Background(), sessID) })
	_ = launched

	argv := strings.Split(strings.TrimSpace(string(waitForFile(t, argvFile))), "\n")
	if slices.Contains(argv, testBootPrompt) {
		t.Errorf("boot prompt passed in argv: %q", argv)
	}
	assertNoRepeatedFlags(t, argv)
	for _, flag := range []string{"--input-format", "--mcp-config", "--add-dir", "--permission-mode"} {
		if !slices.Contains(argv, flag) {
			t.Errorf("spawned argv missing %s: %q", flag, argv)
		}
	}
	if i := slices.Index(argv, "--permission-mode"); i < 0 || i+1 >= len(argv) || argv[i+1] != "bypassPermissions" {
		t.Errorf("spawned argv does not run under bypassPermissions: %q", argv)
	}
	assertStreamJSONUserTurn(t, waitForFile(t, stdinFile), testBootPrompt)
}

// assertPromptLast checks that nothing follows a turn's prompt but what the
// convention puts there: when argv carries the end-of-options "--", the
// prompt is the one and only argument after it, so no launch-only argument
// (--add-dir, --cd, --mcp-config) is read as prompt text or a positional.
func assertPromptLast(t *testing.T, argv []string, prompt string) {
	t.Helper()
	i := slices.Index(argv, "--")
	if i < 0 {
		if prompt != "" && slices.Contains(argv, prompt) {
			t.Errorf("prompt %q is in argv with no end-of-options marker: %q", prompt, argv)
		}
		return
	}
	if got := argv[i+1:]; len(got) != 1 || got[0] != prompt {
		t.Errorf("after \"--\": %q, want only the prompt %q (argv %q)", got, prompt, argv)
	}
}

// End to end through LaunchSession and agentkit's adapter runtime with the
// v0.34.1 conventions: a codex exec turn whose text looks like a flag reaches
// the CLI as the prompt after "--" (CW-20261001-0069), and the launch's --cd
// sits before the marker instead of after the prompt.
func TestLaunchSession_CodexExecPromptAfterEndOfOptions(t *testing.T) {
	dir := t.TempDir()
	argvFile := filepath.Join(dir, "argv")
	fake := filepath.Join(dir, "codex")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argvFile + "\n" +
		"echo '{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}'\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil { //nolint:gosec // test stand-in must be executable
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := t.TempDir()
	const sessID = "sess-codex-exec-dashdash"
	plan := &launch.Plan{
		LaunchID:       "codex-launch",
		ProjectID:      "proj",
		LogicalAgentID: "agent",
		ProviderID:     "codex-cli",
		ProviderBrand:  "codex",
		RuntimeKind:    config.RuntimeKindSubprocess,
		RepoRoot:       repo,
		WriteHome:      t.TempDir(),
		WorkspaceMode:  "shared",
		Command:        fake,
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
	svc := &Service{
		CatalogRoot: t.TempDir(),
		Catalog:     &config.Catalog{Global: config.Global{Version: "test"}},
		Store:       db,
		Manager:     agentsessions.NewManager(stateSinkAdapter{db: db}),
		factories:   map[string]RuntimeFactory{"codex-cli": factory},
	}
	if _, err := svc.LaunchSession(sessID); err != nil {
		t.Fatalf("LaunchSession: %v", err)
	}
	t.Cleanup(func() { _ = svc.Manager.Stop(context.Background(), sessID) })

	const prompt = "--bogus-smoke-flag"
	if err := svc.SendTurn(context.Background(), sessID, prompt); err != nil {
		t.Fatalf("SendTurn: %v", err)
	}
	argv := strings.Split(strings.TrimSpace(string(waitForFile(t, argvFile))), "\n")
	assertPromptLast(t, argv, prompt)
	cd := slices.Index(argv, "--cd")
	if cd < 0 || cd+1 >= len(argv) || argv[cd+1] != repo || cd > slices.Index(argv, "--") {
		t.Fatalf("argv lacks --cd %s before \"--\": %q", repo, argv)
	}
}

func assertStreamJSONUserTurn(t *testing.T, payload []byte, want string) {
	t.Helper()
	var frame struct {
		Type    string `json:"type"`
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(payload, &frame); err != nil {
		t.Fatalf("first turn is not a stream-json frame: %v (%q)", err, payload)
	}
	if frame.Type != "user" || frame.Message.Role != "user" || frame.Message.Content != want {
		t.Fatalf("first turn = %+v, want a user message carrying the boot prompt", frame)
	}
}

func waitForFile(t *testing.T, path string) []byte {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		data, err := os.ReadFile(path) //nolint:gosec // test-owned temp path
		if err == nil {
			return data
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", path)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func assertNoRepeatedFlags(t *testing.T, argv []string) {
	t.Helper()
	seen := map[string]bool{}
	for _, tok := range argv {
		// codex's -c is one config override per occurrence (the posture
		// sets sandbox_mode and approval_policy with two).
		if tok == "-c" {
			continue
		}
		if strings.HasPrefix(tok, "-") && !seen[tok] && countToken(argv, tok) > 1 {
			t.Errorf("flag %q repeated in argv: %q", tok, argv)
		}
		seen[tok] = true
	}
}

func countToken(argv []string, tok string) int {
	n := 0
	for _, a := range argv {
		if a == tok {
			n++
		}
	}
	return n
}
