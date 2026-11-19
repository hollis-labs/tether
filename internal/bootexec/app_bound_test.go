package bootexec

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launch"
)

// The checked-in static seat resolves to an app-only proxy grant. The generated
// Claude config plants that proxy, whose confined catalog contains Surface /mcp.
func TestAppBoundSeatMCPConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root, err := filepath.Abs(filepath.Join("..", "..", "examples", "app-bound-seats", "catalog"))
	if err != nil {
		t.Fatal(err)
	}
	cat, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := launch.Resolve(cat, launch.Input{LaunchID: "surface-claude", CatalogRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if plan.LogicalAgentID != "surface-assistant" || plan.Env[launch.MCPServersEnv] != "surface" {
		t.Fatalf("seat grant: %s %v", plan.LogicalAgentID, plan.Env)
	}
	entries, unknown, err := config.LoadMCPServersConfined(root, launch.EffectiveMCPServers(plan.Env))
	if err != nil || len(unknown) != 0 || len(entries) != 1 {
		t.Fatalf("app MCP config = %v, unknown %v, %v", entries, unknown, err)
	}
	if app := entries[0]; app.ID != "surface" || app.Transport != "http" || app.URL != "http://127.0.0.1:8080/mcp" {
		t.Fatalf("app upstream = %+v", app)
	}
	plan.RepoRoot = t.TempDir()
	prepared, err := PrepareClaudeTUI(plan, Options{BootDirRoot: t.TempDir(), TetherCommand: "tether", TetherArgs: []string{"--catalog", root, "mcp", "--proxy"}, TetherEnv: []string{"TETHER_MCP_SERVERS=" + plan.Env[launch.MCPServersEnv]}, ParentEnv: []string{"PATH=/bin"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(prepared.BootDir) })
	var planted struct {
		Servers map[string]struct {
			Command string            `json:"command"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(prepared.BootDir, ".mcp.json"))), &planted); err != nil {
		t.Fatal(err)
	}
	proxy, ok := planted.Servers["mux"]
	if !ok || proxy.Command != "tether" || proxy.Env[launch.MCPServersEnv] != "surface" {
		t.Fatalf("planted MCP config = %+v", planted)
	}
}
