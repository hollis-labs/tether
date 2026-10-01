package app

// CW-20261001-0145: an agent_file/agent_inline override's default_sandbox is
// applied at launch, or the launch is refused. It is never silently dropped
// in favor of the catalog agent's looser profile.

import (
	"errors"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/config"
)

func TestApplyAgentOps_RecordsOverrideSandboxOnPlan(t *testing.T) {
	for _, tc := range []struct {
		name, catalog, inline, want string
	}{
		{"override tightens an unsandboxed agent", "", `{"permissions":{"default_sandbox":"workspace-only"}}`, "workspace-only"},
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

// Interim rule (until CW-20260930-0253): an override may not change a
// catalog agent's pinned sandbox profile, since an agent holding
// session.write could otherwise loosen its own sandbox in a child session.
func TestApplyAgentOps_OverrideCannotChangePinnedSandbox(t *testing.T) {
	svc := buildTestService(t, map[string]config.Agent{
		"test-agent": {ID: "test-agent", Permissions: config.AgentPermissions{DefaultSandbox: "workspace-only"}},
	}, t.TempDir())
	plan := basePlan()
	err := svc.applyAgentOps(plan, CreateSessionInput{LaunchID: "test-launch", AgentInline: `{"permissions":{"default_sandbox":"unrestricted"}}`})
	if !errors.Is(err, config.ErrSandboxOverrideRefused) {
		t.Fatalf("err = %v; want ErrSandboxOverrideRefused", err)
	}
	for _, want := range []string{`"workspace-only"`, `"unrestricted"`, "CW-20260930-0253"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %s", err, want)
		}
	}
	if plan.SandboxProfile != "" {
		t.Fatalf("plan.SandboxProfile = %q after a refused override", plan.SandboxProfile)
	}
}

// An unknown profile is still a 404-class refusal, checked first, even over a
// pinned agent.
func TestApplyAgentOps_UnknownOverrideOverPinnedIsUnknown(t *testing.T) {
	svc := buildTestService(t, map[string]config.Agent{
		"test-agent": {ID: "test-agent", Permissions: config.AgentPermissions{DefaultSandbox: "workspace-only"}},
	}, t.TempDir())
	err := svc.applyAgentOps(basePlan(), CreateSessionInput{LaunchID: "test-launch", AgentInline: `{"permissions":{"default_sandbox":"gone"}}`})
	if !errors.Is(err, config.ErrUnknownSandboxProfile) {
		t.Fatalf("err = %v; want ErrUnknownSandboxProfile", err)
	}
}

// If the catalog pins the agent after create, a recorded override that
// differs is refused at launch too.
func TestLaunchSession_OverrideOfNowPinnedAgentRefused(t *testing.T) {
	l := launchWithPlanSandbox(t, "workspace-only", "unrestricted")
	if !errors.Is(l.err, config.ErrSandboxOverrideRefused) {
		t.Fatalf("LaunchSession err = %v; want ErrSandboxOverrideRefused", l.err)
	}
	if l.started {
		t.Fatal("runtime started with an overridden pinned sandbox")
	}
}
