package app

import (
	"encoding/json"
	"github.com/hollis-labs/tether/internal/bootgen"
	"github.com/hollis-labs/tether/internal/mcpgateway"
	"os"
	"path/filepath"
	"testing"
)

func TestBootToolsCapturedAndPlanted(t *testing.T) {
	for _, fragment := range []string{"", "mcp_tools: []\n", "mcp_tools: [torque_task_get]\n"} {
		t.Run(fragment, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "profile.yaml")
			if err := os.WriteFile(path, []byte("id: test\n"+fragment), 0600); err != nil {
				t.Fatal(err)
			}
			profile, err := bootgen.LoadProfile(path)
			if err != nil {
				t.Fatal(err)
			}
			plan := basePlan()
			applyMCPAllowlist(plan, profile)
			captured, err := sessionMCPPolicy("s", "a", plan, mcpgateway.Config{})
			if err != nil {
				t.Fatal(err)
			}
			_, planted := tetherEnvMap(plan.Env)[mcpgateway.ToolsEnv]
			if (captured.ToolProfile != nil) != (fragment != "") || planted != (fragment != "") {
				t.Fatal("omission/empty policy changed at planting")
			}
			if profile.MCPTools != nil {
				want, _ := json.Marshal(profile.MCPTools)
				if plan.Env[mcpgateway.ToolsEnv] != string(want) {
					t.Fatal("boot grant lost")
				}
			}
		})
	}
}
