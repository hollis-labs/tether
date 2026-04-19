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
