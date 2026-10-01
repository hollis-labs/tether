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

	"github.com/hollis-labs/agentkit/agentlaunch"
	"github.com/hollis-labs/agentkit/agentsessions"
	gop "github.com/hollis-labs/go-providers/provider"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/provider/cli/claudestream"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/workspace"
)

const testBootPrompt = "BOOT PROMPT: read the task and begin"

// CW-20261001-0015: the shared launch's argv and the runtime adapter's argv
// were both handed to the spawn. The argv a runtime composes — plan.Args, its
// adapter's convention, then ExtraArgs — must name each flag and subcommand
// once, keep the boot prompt out of argv, and still carry the args only the
// launch knows (the planted --mcp-config, the project dir).
func TestSharedExtraArgs_ComposesArgvOnce(t *testing.T) {
	for _, tc := range []struct {
		name        string
		providerID  string
		brand       string
		runtimeKind string
		args        []string
		adapter     gop.CLIAdapter
		turnPrompt  string
		wantPairs   [][2]string // flag, root ("boot" | "project")
		wantOnce    []string
	}{
		{
			name:        "claude streaming-stdio",
			providerID:  "claude-code",
			brand:       "claude",
			runtimeKind: config.RuntimeKindStreamingStdio,
			adapter:     gop.NewClaudeAdapterStreamingStdio(),
			wantPairs:   [][2]string{{"--mcp-config", "boot"}, {"--add-dir", "project"}},
			wantOnce:    []string{"-p", "--input-format", "--output-format", "--verbose"},
		},
		{
			name:        "claude streaming-stdio with provider args",
			providerID:  "claude-code",
			brand:       "claude",
			runtimeKind: config.RuntimeKindStreamingStdio,
			args:        []string{"--model", "opus", "--dangerously-skip-permissions"},
			adapter:     gop.NewClaudeAdapterStreamingStdio(),
			wantPairs:   [][2]string{{"--mcp-config", "boot"}, {"--add-dir", "project"}},
			wantOnce:    []string{"-p", "--model", "opus", "--dangerously-skip-permissions"},
		},
		{
			// launch.Resolve's claude args: --mcp-config relative to the boot
			// dir names the same file the projection passes absolute.
			name:        "claude streaming-stdio with resolver args",
			providerID:  "claude-code",
			brand:       "claude",
			runtimeKind: config.RuntimeKindStreamingStdio,
			args:        []string{"--mcp-config", ".mcp.json", "--dangerously-skip-permissions"},
			adapter:     gop.NewClaudeAdapterStreamingStdio(),
			wantPairs:   [][2]string{{"--add-dir", "project"}},
			wantOnce:    []string{"-p", "--mcp-config", ".mcp.json", "--dangerously-skip-permissions"},
		},
		{
			name:        "codex app-server",
			providerID:  "codex-app-server",
			brand:       "codex",
			runtimeKind: config.RuntimeKindJSONRPCStdio,
			adapter:     gop.NewCodexAdapterAppServer(),
			wantOnce:    []string{"app-server"},
		},
		{
			// Per-turn print mode: the prompt rides after "--" (go-providers
			// v0.34.1), and the launch's --add-dir must come before it.
			name:        "claude print turn",
			providerID:  "claude-sub",
			brand:       "claude",
			runtimeKind: config.RuntimeKindSubprocess,
			args:        []string{"--mcp-config", ".mcp.json", "--dangerously-skip-permissions"},
			adapter:     gop.NewClaudeAdapter(),
			turnPrompt:  "--dangerously-bypass-everything",
			wantPairs:   [][2]string{{"--add-dir", "project"}},
			wantOnce:    []string{"-p", "--mcp-config", "--output-format", "--dangerously-skip-permissions"},
		},
		{
			name:        "codex-cli exec",
			providerID:  "codex-cli",
			brand:       "codex",
			runtimeKind: config.RuntimeKindSubprocess,
			adapter:     gop.NewCodexAdapter(),
			turnPrompt:  "TURN PROMPT",
			wantPairs:   [][2]string{{"--cd", "project"}},
			wantOnce:    []string{"exec", "TURN PROMPT", "--json", "--skip-git-repo-check"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := &Service{
				CatalogRoot: t.TempDir(),
				Catalog:     &config.Catalog{Global: config.Global{Version: "test"}},
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
				BootPrompt:     testBootPrompt,
			}
			prepared, err := svc.prepareSharedLaunch(context.Background(), plan, ws, plantContextInput{
				MuxCommand: "mux",
				MuxArgs:    []string{"mcp"},
			})
			if err != nil {
				t.Fatalf("prepareSharedLaunch: %v", err)
			}

			// Composed the way LaunchSession hands the extras to the runtime.
			scoped := &claudestream.PlanScopedAdapter{Inner: tc.adapter, BaseArgs: plan.Args}
			scoped.SetExtraArgs(sharedExtraArgs(plan.ProviderBrand, prepared, plan.Args))
			argv := scoped.BuildArgs(tc.turnPrompt, "", "")
			assertPromptLast(t, argv, tc.turnPrompt)

			if slices.Contains(argv, testBootPrompt) {
				t.Errorf("boot prompt is in argv: %q", argv)
			}
			for _, tok := range tc.wantOnce {
				if n := countToken(argv, tok); n != 1 {
					t.Errorf("%q appears %d times, want 1: %q", tok, n, argv)
				}
			}
			assertNoRepeatedFlags(t, argv)
			roots := map[string]string{"boot": prepared.PlantedBootDir, "project": repo}
			for _, pair := range tc.wantPairs {
				i := slices.Index(argv, pair[0])
				if i < 0 || i+1 >= len(argv) {
					t.Errorf("%s missing: %q", pair[0], argv)
					continue
				}
				if !underAnyRoot(argv[i+1], []string{roots[pair[1]]}) {
					t.Errorf("%s %q is not under the %s root %q", pair[0], argv[i+1], pair[1], roots[pair[1]])
				}
			}
		})
	}
}

func TestSharedExtraArgs_CodexAppServerIsOneSubcommand(t *testing.T) {
	svc := &Service{
		CatalogRoot: t.TempDir(),
		Catalog:     &config.Catalog{Global: config.Global{Version: "test"}},
	}
	ws := t.TempDir()
	plan := &launch.Plan{
		LaunchID:      "demo",
		ProjectID:     "project",
		ProviderID:    "codex-app-server",
		ProviderBrand: "codex",
		RuntimeKind:   config.RuntimeKindJSONRPCStdio,
		RepoRoot:      t.TempDir(),
		WriteHome:     ws,
		WorkspaceMode: "shared",
		Command:       "codex",
		BootPrompt:    testBootPrompt,
	}
	prepared, err := svc.prepareSharedLaunch(context.Background(), plan, ws, plantContextInput{MuxCommand: "mux"})
	if err != nil {
		t.Fatalf("prepareSharedLaunch: %v", err)
	}
	scoped := &claudestream.PlanScopedAdapter{Inner: gop.NewCodexAdapterAppServer()}
	scoped.SetExtraArgs(sharedExtraArgs(plan.ProviderBrand, prepared, plan.Args))
	argv := scoped.BuildArgs("", "", "")
	if !slices.Equal(argv, []string{"app-server"}) {
		t.Fatalf("argv = %q, want [app-server]", argv)
	}
}

// Providers outside claude/codex keep the old pass-through until the single
// argv owner (CW-20260930-0135) lands.
func TestSharedExtraArgs_OtherProvidersPassThrough(t *testing.T) {
	prepared := preparedWithArgv("opencode", "run", "--agent", "agent", "--dir", "/p", "boot")
	got := sharedExtraArgs("opencode", prepared, nil)
	want := []string{"run", "--agent", "agent", "--dir", "/p", "boot"}
	if !slices.Equal(got, want) {
		t.Fatalf("sharedExtraArgs = %q, want %q", got, want)
	}
	got = sharedExtraArgs("opencode", preparedWithArgv("opencode", "--x", "run"), []string{"--x"})
	if !slices.Equal(got, []string{"run"}) {
		t.Fatalf("sharedExtraArgs with base args = %q, want [run]", got)
	}
}

func TestRootBoundArgs(t *testing.T) {
	roots := []string{"/boot", "/proj"}
	args := []string{"-p", "prompt", "--verbose", "--mcp-config", "/boot/.mcp.json", "--add-dir", "/proj", "/proj/x", "--cd", "/elsewhere", "/bootstrap"}
	got := rootBoundArgs(args, roots, nil)
	want := []string{"--mcp-config", "/boot/.mcp.json", "--add-dir", "/proj", "/proj/x"}
	if !slices.Equal(got, want) {
		t.Fatalf("rootBoundArgs = %q, want %q", got, want)
	}

	// A pair plan.Args already carries — same flag, same target — is not
	// passed again; the same flag at another target still is.
	have := flagTargets([]string{"--mcp-config", ".mcp.json", "--add-dir", "/other"}, "/boot")
	got = rootBoundArgs(args, roots, have)
	want = []string{"--add-dir", "/proj", "/proj/x"}
	if !slices.Equal(got, want) {
		t.Fatalf("rootBoundArgs with plan.Args pairs = %q, want %q", got, want)
	}
}

func TestStreamingStdioBootPromptFirstTurn(t *testing.T) {
	for _, mode := range []string{"planted", "stdin", ""} {
		t.Run("mode="+mode, func(t *testing.T) {
			opts := agentsessions.StartOptions{BootPrompt: testBootPrompt, BootMode: mode}
			streamingStdioBootPromptFirstTurn(agentsessions.Capabilities{StreamingStdio: true}, &opts)

			if opts.BootPrompt != "" || opts.BootMode != "" {
				t.Fatalf("BootPrompt/BootMode = %q/%q, want cleared so agentkit does not also write it raw", opts.BootPrompt, opts.BootMode)
			}
			if !opts.AutoFireFirstTurn {
				t.Fatal("AutoFireFirstTurn = false, want true")
			}
			assertStreamJSONUserTurn(t, opts.FirstTurnPayload, testBootPrompt)
		})
	}
}

func TestStreamingStdioBootPromptFirstTurn_Unchanged(t *testing.T) {
	for _, tc := range []struct {
		name string
		caps agentsessions.Capabilities
		opts agentsessions.StartOptions
	}{
		{"pty runtime", agentsessions.Capabilities{PTY: true}, agentsessions.StartOptions{BootPrompt: "p", BootMode: "planted"}},
		{"jsonrpc runtime", agentsessions.Capabilities{JsonRpcStdio: true}, agentsessions.StartOptions{BootPrompt: "p", BootMode: "planted"}},
		{"boot mode none", agentsessions.Capabilities{StreamingStdio: true}, agentsessions.StartOptions{BootPrompt: "p", BootMode: "none"}},
		{"no boot prompt", agentsessions.Capabilities{StreamingStdio: true}, agentsessions.StartOptions{BootMode: "planted"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := tc.opts
			streamingStdioBootPromptFirstTurn(tc.caps, &opts)
			if opts.BootPrompt != tc.opts.BootPrompt || opts.BootMode != tc.opts.BootMode || opts.AutoFireFirstTurn {
				t.Fatalf("options changed: %+v", opts)
			}
		})
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
		Args:           []string{"--mcp-config", ".mcp.json", "--dangerously-skip-permissions"},
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
	for _, flag := range []string{"--input-format", "--mcp-config", "--add-dir"} {
		if !slices.Contains(argv, flag) {
			t.Errorf("spawned argv missing %s: %q", flag, argv)
		}
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

func preparedWithArgv(argv ...string) *agentlaunch.PreparedLaunch {
	return &agentlaunch.PreparedLaunch{Argv: argv}
}

func TestPeelIndex(t *testing.T) {
	seg := []string{"--mcp-config", ".mcp.json"}
	for _, tc := range []struct {
		name string
		argv []string
		want int
	}{
		{"before the marker (agentkit v0.12.3)", []string{"-p", "--verbose", "--mcp-config", ".mcp.json", "--", "boot"}, 2},
		{"at the end (earlier agentkit)", []string{"-p", "--", "boot", "--mcp-config", ".mcp.json"}, 3},
		{"no marker, at the end", []string{"app-server", "--mcp-config", ".mcp.json"}, 1},
		{"absent", []string{"-p", "--", "boot"}, -1},
	} {
		if got := peelIndex(tc.argv, seg); got != tc.want {
			t.Errorf("%s: peelIndex = %d, want %d", tc.name, got, tc.want)
		}
	}
}
