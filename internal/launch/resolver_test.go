package launch

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/config"
)

func TestResolveDemoLaunch(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	catalogRoot := filepath.Join(filepath.Dir(file), "..", "..", "examples", "catalog")
	cat, err := config.Load(catalogRoot)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	plan, err := Resolve(cat, Input{LaunchID: "demo-launch", CatalogRoot: catalogRoot})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if plan.Command != "claude" {
		t.Fatalf("want command=claude, got %q", plan.Command)
	}
	if plan.LogicalAgentID != "demo-agent" {
		t.Fatalf("want LogicalAgentID=demo-agent, got %q", plan.LogicalAgentID)
	}
	if plan.ProviderBrand != "claude" {
		t.Fatalf("want ProviderBrand=claude, got %q", plan.ProviderBrand)
	}
	if plan.RuntimeKind != config.RuntimeKindStreamingStdio {
		t.Fatalf("want RuntimeKind=%q, got %q", config.RuntimeKindStreamingStdio, plan.RuntimeKind)
	}
	if !strings.Contains(plan.BootPrompt, "Tether") {
		t.Fatalf("boot prompt missing common fragment: %q", plan.BootPrompt)
	}
}

// TestResolve_EnvPolicyCarriedIntoPlan proves the resolver stops pre-flattening
// parent env values and instead carries only overrides + the provider policy
// (mode / passthrough / redact). The adapter composes the effective env at
// Build time.
func TestResolve_EnvPolicyCarriedIntoPlan(t *testing.T) {
	cat := &config.Catalog{
		Projects: map[string]config.Project{
			"demo": {ID: "demo", RepoRoot: "/tmp/demo", Workspace: config.WorkspaceSpec{SessionRoot: "/tmp/sessions"}},
		},
		Agents: map[string]config.Agent{
			"demo-agent": {ID: "demo-agent"},
		},
		Providers: map[string]config.Provider{
			"merge-prov": {
				ID:      "merge-prov",
				Command: "echo",
				Env:     config.ProviderEnv{Mode: "", Redact: []string{"SECRET"}},
			},
			"whitelist-prov": {
				ID:      "whitelist-prov",
				Command: "echo",
				Env:     config.ProviderEnv{Mode: "whitelist", Passthrough: []string{"PATH", "HOME"}},
			},
		},
		Launches: map[string]config.Launch{
			"merge-launch":     {ID: "merge-launch", Project: "demo", Agent: "demo-agent", Provider: "merge-prov", Overrides: config.LaunchOverrides{Env: map[string]string{"FOO": "bar"}}},
			"whitelist-launch": {ID: "whitelist-launch", Project: "demo", Agent: "demo-agent", Provider: "whitelist-prov"},
		},
	}

	t.Run("default mode becomes merge when unset", func(t *testing.T) {
		plan, err := Resolve(cat, Input{LaunchID: "merge-launch"})
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if plan.EnvMode != "merge" {
			t.Fatalf("want EnvMode=merge, got %q", plan.EnvMode)
		}
		if plan.Env["FOO"] != "bar" || len(plan.Env) != 1 {
			t.Fatalf("plan.Env should contain only overrides; got %v", plan.Env)
		}
		if !equalStringSlices(plan.EnvRedact, []string{"SECRET"}) {
			t.Fatalf("EnvRedact should carry from provider; got %v", plan.EnvRedact)
		}
	})

	t.Run("api-stub-launch resolves against example catalog", func(t *testing.T) {
		_, file, _, _ := runtime.Caller(0)
		catalogRoot := filepath.Join(filepath.Dir(file), "..", "..", "examples", "catalog")
		cat, err := config.Load(catalogRoot)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		plan, err := Resolve(cat, Input{LaunchID: "api-stub-launch", CatalogRoot: catalogRoot})
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if plan.ProviderID != "api-stub" {
			t.Errorf("want ProviderID=api-stub, got %q", plan.ProviderID)
		}
		if plan.Command != "" {
			t.Errorf("api-stub plan.Command should be empty, got %q", plan.Command)
		}
	})

	t.Run("whitelist mode preserves passthrough list", func(t *testing.T) {
		plan, err := Resolve(cat, Input{LaunchID: "whitelist-launch"})
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if plan.EnvMode != "whitelist" {
			t.Fatalf("want EnvMode=whitelist, got %q", plan.EnvMode)
		}
		if !equalStringSlices(plan.EnvPassthrough, []string{"PATH", "HOME"}) {
			t.Fatalf("EnvPassthrough should carry from provider; got %v", plan.EnvPassthrough)
		}
		if len(plan.Env) != 0 {
			t.Fatalf("plan.Env should be empty when no overrides; got %v", plan.Env)
		}
	})
}

// TestResolve_MCPServerChain verifies the config-chain precedence:
// project.mcp.servers > launch.mcp.servers > (nothing → no env var).
func TestResolve_MCPServerChain(t *testing.T) {
	base := func() *config.Catalog {
		return &config.Catalog{
			Projects: map[string]config.Project{
				"proj": {ID: "proj", RepoRoot: "/tmp/p", Workspace: config.WorkspaceSpec{SessionRoot: "/tmp/ws"}},
			},
			Agents:    map[string]config.Agent{"a": {ID: "a"}},
			Providers: map[string]config.Provider{"p": {ID: "p", Command: "echo"}},
			Launches:  map[string]config.Launch{},
		}
	}

	t.Run("neither project nor launch sets servers", func(t *testing.T) {
		cat := base()
		cat.Launches["l"] = config.Launch{ID: "l", Project: "proj", Agent: "a", Provider: "p"}
		plan, err := Resolve(cat, Input{LaunchID: "l"})
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if _, ok := plan.Env["MUX_MCP_SERVERS"]; ok {
			t.Fatalf("MUX_MCP_SERVERS should not be set when no servers configured; got %q", plan.Env["MUX_MCP_SERVERS"])
		}
	})

	t.Run("launch sets servers", func(t *testing.T) {
		cat := base()
		cat.Launches["l"] = config.Launch{
			ID: "l", Project: "proj", Agent: "a", Provider: "p",
			MCP: config.MCPConfig{Servers: []string{"hadron", "vanta"}},
		}
		plan, err := Resolve(cat, Input{LaunchID: "l"})
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if plan.Env["MUX_MCP_SERVERS"] != "hadron,vanta" {
			t.Fatalf("want MUX_MCP_SERVERS=hadron,vanta, got %q", plan.Env["MUX_MCP_SERVERS"])
		}
	})

	t.Run("project overrides launch servers", func(t *testing.T) {
		cat := base()
		proj := cat.Projects["proj"]
		proj.MCP = config.MCPConfig{Servers: []string{"cerberus"}}
		cat.Projects["proj"] = proj
		cat.Launches["l"] = config.Launch{
			ID: "l", Project: "proj", Agent: "a", Provider: "p",
			MCP: config.MCPConfig{Servers: []string{"hadron", "vanta"}},
		}
		plan, err := Resolve(cat, Input{LaunchID: "l"})
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if plan.Env["MUX_MCP_SERVERS"] != "cerberus" {
			t.Fatalf("want project servers to win; got %q", plan.Env["MUX_MCP_SERVERS"])
		}
	})

	t.Run("project sets servers, launch has none", func(t *testing.T) {
		cat := base()
		proj := cat.Projects["proj"]
		proj.MCP = config.MCPConfig{Servers: []string{"hadron"}}
		cat.Projects["proj"] = proj
		cat.Launches["l"] = config.Launch{ID: "l", Project: "proj", Agent: "a", Provider: "p"}
		plan, err := Resolve(cat, Input{LaunchID: "l"})
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if plan.Env["MUX_MCP_SERVERS"] != "hadron" {
			t.Fatalf("want MUX_MCP_SERVERS=hadron, got %q", plan.Env["MUX_MCP_SERVERS"])
		}
	})
}

func TestResolve_PermissionMode(t *testing.T) {
	// argv contains the flag value pair "<flag> <value>" in order.
	hasFlag := func(args []string, flag, value string) bool {
		for i := 0; i+1 < len(args); i++ {
			if args[i] == flag && args[i+1] == value {
				return true
			}
		}
		return false
	}
	base := func(brand, globalMode, agentMode string) *config.Catalog {
		cat := &config.Catalog{
			Projects: map[string]config.Project{
				"proj": {ID: "proj", RepoRoot: "/tmp/p", Workspace: config.WorkspaceSpec{SessionRoot: "/tmp/ws"}},
			},
			Agents: map[string]config.Agent{
				"a": {ID: "a", Permissions: config.AgentPermissions{PermissionMode: agentMode}},
			},
			Providers: map[string]config.Provider{
				"p": {ID: "p", Command: "echo", Provider: brand},
			},
			Launches: map[string]config.Launch{
				"l": {ID: "l", Project: "proj", Agent: "a", Provider: "p"},
			},
		}
		cat.Global.Catalog.Defaults.PermissionMode = globalMode
		return cat
	}

	t.Run("claude + global bypass → skip-permissions + mcp-config", func(t *testing.T) {
		plan, err := Resolve(base("claude", config.PermissionModeBypass, ""), Input{LaunchID: "l"})
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if plan.PermissionMode != config.PermissionModeBypass {
			t.Fatalf("PermissionMode = %q, want bypass", plan.PermissionMode)
		}
		if !strings.Contains(strings.Join(plan.Args, " "), "--dangerously-skip-permissions") {
			t.Errorf("args missing --dangerously-skip-permissions: %v", plan.Args)
		}
		if !hasFlag(plan.Args, "--mcp-config", ".mcp.json") {
			t.Errorf("args missing --mcp-config .mcp.json: %v", plan.Args)
		}
	})

	t.Run("claude + agent override default beats global bypass", func(t *testing.T) {
		plan, err := Resolve(base("claude", config.PermissionModeBypass, config.PermissionModeDefault), Input{LaunchID: "l"})
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if plan.PermissionMode != config.PermissionModeDefault {
			t.Fatalf("PermissionMode = %q, want default", plan.PermissionMode)
		}
		if strings.Contains(strings.Join(plan.Args, " "), "--dangerously-skip-permissions") {
			t.Errorf("default mode must not emit --dangerously-skip-permissions: %v", plan.Args)
		}
		// --mcp-config is independent of permission mode — always on for claude.
		if !hasFlag(plan.Args, "--mcp-config", ".mcp.json") {
			t.Errorf("args missing --mcp-config .mcp.json: %v", plan.Args)
		}
	})

	t.Run("claude + nothing set → conservative default fallback", func(t *testing.T) {
		plan, err := Resolve(base("claude", "", ""), Input{LaunchID: "l"})
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if plan.PermissionMode != config.PermissionModeDefault {
			t.Fatalf("PermissionMode = %q, want default fallback", plan.PermissionMode)
		}
	})

	t.Run("non-claude provider gets no claude flags", func(t *testing.T) {
		plan, err := Resolve(base("codex", config.PermissionModeBypass, ""), Input{LaunchID: "l"})
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if plan.PermissionMode != config.PermissionModeBypass {
			t.Fatalf("PermissionMode = %q, want bypass (recorded even for non-claude)", plan.PermissionMode)
		}
		joined := strings.Join(plan.Args, " ")
		if strings.Contains(joined, "--dangerously-skip-permissions") || strings.Contains(joined, "--mcp-config") {
			t.Errorf("non-claude provider must not get claude CLI flags: %v", plan.Args)
		}
	})
}

func TestResolve_InjectionFiles(t *testing.T) {
	catalogRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(catalogRoot, "context.md"), []byte("from source\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cat := &config.Catalog{
		Projects: map[string]config.Project{
			"proj": {ID: "proj", RepoRoot: "/tmp/p", Workspace: config.WorkspaceSpec{SessionRoot: "/tmp/ws"}},
		},
		Agents:    map[string]config.Agent{"a": {ID: "a"}},
		Providers: map[string]config.Provider{"p": {ID: "p", Command: "echo"}},
		Launches: map[string]config.Launch{
			"l": {
				ID: "l", Project: "proj", Agent: "a", Provider: "p",
				Injection: config.LaunchInjection{
					NativeFiles: []config.InjectedFile{
						{RelPath: ".mux/inline.md", Content: "inline\n", Mode: 0o600},
						{RelPath: ".mux/context.md", Source: "context.md"},
					},
					BootDirOverlay: []config.InjectedFile{
						{RelPath: "extra.md", Content: "overlay\n"},
					},
				},
			},
		},
	}
	plan, err := Resolve(cat, Input{LaunchID: "l", CatalogRoot: catalogRoot})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(plan.NativeFiles) != 2 {
		t.Fatalf("NativeFiles len = %d, want 2", len(plan.NativeFiles))
	}
	if plan.NativeFiles[0].Kind != "raw" || plan.NativeFiles[0].RelPath != ".mux/inline.md" || plan.NativeFiles[0].Content != "inline\n" || plan.NativeFiles[0].Mode != 0o600 {
		t.Fatalf("unexpected inline native file: %#v", plan.NativeFiles[0])
	}
	if plan.NativeFiles[1].Content != "from source\n" {
		t.Fatalf("source native file content = %q", plan.NativeFiles[1].Content)
	}
	if plan.BootDirOverlay["extra.md"] != "overlay\n" {
		t.Fatalf("boot overlay = %#v", plan.BootDirOverlay)
	}
}

func TestResolve_InjectionRejectsDuplicateBootOverlayPaths(t *testing.T) {
	cat := &config.Catalog{
		Projects: map[string]config.Project{
			"proj": {ID: "proj", RepoRoot: "/tmp/p", Workspace: config.WorkspaceSpec{SessionRoot: "/tmp/ws"}},
		},
		Agents:    map[string]config.Agent{"a": {ID: "a"}},
		Providers: map[string]config.Provider{"p": {ID: "p", Command: "echo"}},
		Launches: map[string]config.Launch{
			"l": {
				ID: "l", Project: "proj", Agent: "a", Provider: "p",
				Injection: config.LaunchInjection{
					BootDirOverlay: []config.InjectedFile{
						{RelPath: "extra.md", Content: "one\n"},
						{RelPath: "extra.md", Content: "two\n"},
					},
				},
			},
		},
	}
	if _, err := Resolve(cat, Input{LaunchID: "l", CatalogRoot: t.TempDir()}); err == nil {
		t.Fatal("expected duplicate boot overlay rel_path to fail")
	}
}

func TestResolve_SkipPromptFragmentsAllowsGeneratedBootReplacement(t *testing.T) {
	cat := &config.Catalog{
		Projects: map[string]config.Project{
			"demo": {
				ID:            "demo",
				RepoRoot:      "/tmp/demo",
				BootFragments: []string{"missing/.agentrc/boot-prompt.md"},
				Workspace:     config.WorkspaceSpec{SessionRoot: "/tmp/sessions"},
			},
		},
		Agents: map[string]config.Agent{
			"demo-agent": {ID: "demo-agent"},
		},
		Providers: map[string]config.Provider{
			"provider": {ID: "provider", Command: "echo"},
		},
		Launches: map[string]config.Launch{
			"launch": {
				ID:       "launch",
				Project:  "demo",
				Agent:    "demo-agent",
				Provider: "provider",
				Prompt:   config.PromptSpec{IncludeProjectBoot: true},
			},
		},
	}

	if _, err := Resolve(cat, Input{LaunchID: "launch", CatalogRoot: t.TempDir()}); err == nil {
		t.Fatal("expected missing legacy boot fragment to fail without SkipPromptFragments")
	}

	plan, err := Resolve(cat, Input{LaunchID: "launch", CatalogRoot: t.TempDir(), SkipPromptFragments: true})
	if err != nil {
		t.Fatalf("resolve with SkipPromptFragments: %v", err)
	}
	if plan.BootPrompt != "\n" {
		t.Fatalf("BootPrompt = %q, want empty composed prompt newline", plan.BootPrompt)
	}
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
