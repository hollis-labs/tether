package launch

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/chrispian/agent-mux/internal/config"
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
	if !strings.Contains(plan.BootPrompt, "Agent Mux") {
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
