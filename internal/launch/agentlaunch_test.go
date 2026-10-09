package launch

import (
	"testing"

	permission "github.com/hollis-labs/substrate/harness/interception/permission"

	"github.com/hollis-labs/tether/internal/config"
)

// AgentLaunchPlan carries the plan's permission mode to the shared launch as
// a go-permission posture (config.ProviderPosture), and none for an ACP agent.
func TestAgentLaunchPlanCarriesThePosture(t *testing.T) {
	tests := []struct {
		brand, runtime, mode string
		want                 permission.Mode
	}{
		{"claude", config.RuntimeKindStreamingStdio, config.PermissionModeBypass, permission.ModeYolo},
		{"claude", config.RuntimeKindPTY, config.PermissionModeDefault, permission.ModeDefault},
		{"codex", config.RuntimeKindSubprocess, config.PermissionModeBypass, permission.ModeYolo},
		{"opencode", config.RuntimeKindSubprocess, config.PermissionModeBypass, ""},
		{"copilot", config.RuntimeKindACPStdio, config.PermissionModeBypass, ""},
	}
	for _, tc := range tests {
		plan := &Plan{ProviderBrand: tc.brand, RuntimeKind: tc.runtime, PermissionMode: tc.mode}
		if got := AgentLaunchPlan(plan, t.TempDir()).Provider.Permission; got != tc.want {
			t.Errorf("%s %s %s: Provider.Permission = %q, want %q", tc.brand, tc.runtime, tc.mode, got, tc.want)
		}
	}
}
