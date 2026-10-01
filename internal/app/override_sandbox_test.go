package app

// CW-20261001-0145: an agent_file/agent_inline override's default_sandbox is
// applied at launch, or the launch is refused. It is never silently dropped
// in favor of the catalog agent's looser profile.

import (
	"errors"
	"testing"

	"github.com/hollis-labs/tether/internal/config"
)

func TestApplyAgentOps_RecordsOverrideSandboxOnPlan(t *testing.T) {
	for _, tc := range []struct {
		name, catalog, inline, want string
	}{
		{"override tightens an unsandboxed agent", "", `{"permissions":{"default_sandbox":"workspace-only"}}`, "workspace-only"},
		{"override changes the profile", "unrestricted", `{"permissions":{"default_sandbox":"workspace-only"}}`, "workspace-only"},
		{"no override", "workspace-only", ``, ""},
		{"override repeats the catalog profile", "workspace-only", `{"permissions":{"default_sandbox":"workspace-only"}}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := buildTestService(t, map[string]config.Agent{
				"test-agent": {ID: "test-agent", Permissions: config.AgentPermissions{DefaultSandbox: tc.catalog}},
			}, t.TempDir())
			plan := basePlan()
			if err := svc.applyAgentOps(plan, CreateSessionInput{LaunchID: "test-launch", AgentInline: tc.inline}); err != nil {
				t.Fatal(err)
			}
			if plan.SandboxProfile != tc.want {
				t.Fatalf("plan.SandboxProfile = %q; want %q", plan.SandboxProfile, tc.want)
			}
		})
	}
}

// The override's profile reaches the runtime even though the catalog agent
// has none. Before the fix the session ran with no sandbox.
func TestLaunchSession_AppliesOverrideSandbox(t *testing.T) {
	l := launchWithPlanSandbox(t, "", "workspace-only")
	if l.err != nil {
		t.Fatalf("LaunchSession: %v", l.err)
	}
	if !l.started || l.profile.ID != "workspace-only" {
		t.Fatalf("runtime started=%v with profile %q; want the override's workspace-only", l.started, l.profile.ID)
	}
}

// An override profile the catalog no longer defines refuses the launch,
// rather than falling back to the catalog agent's.
func TestLaunchSession_UnknownOverrideSandboxRefused(t *testing.T) {
	l := launchWithPlanSandbox(t, "workspace-only", "gone-profile")
	if !errors.Is(l.err, config.ErrUnknownSandboxProfile) {
		t.Fatalf("LaunchSession err = %v; want ErrUnknownSandboxProfile", l.err)
	}
	if l.started {
		t.Fatal("runtime started without the requested sandbox")
	}
}
