package config

import (
	"slices"
	"testing"

	"github.com/hollis-labs/agent-contracts-leaf/runtimes"
	permission "github.com/hollis-labs/go-permission"
	"github.com/hollis-labs/go-providers/registry"
)

// TestProviderPosture checks default and explicitly unattended launches.
func TestProviderPosture(t *testing.T) {
	tests := []struct {
		mode, brand string
		runtime     runtimes.Mode
		want        permission.Mode
	}{
		{PermissionModeBypass, "claude", runtimes.ModeStreamingStdio, permission.ModeYolo},
		{PermissionModeBypass, "claude", runtimes.ModePTY, permission.ModeYolo},
		{PermissionModeDefault, "claude", runtimes.ModeStreamingStdio, permission.ModeDefault},
		{"", "claude", runtimes.ModeSubprocessPerTurn, permission.ModeDefault},
		{PermissionModeBypass, "codex", runtimes.ModeSubprocessPerTurn, permission.ModeYolo},
		{PermissionModeBypass, "codex", runtimes.ModeJSONRPCStdio, permission.ModeYolo},
		{PermissionModeDefault, "codex", runtimes.ModeSubprocessPerTurn, permission.ModeAcceptEdits},
		{PermissionModeBypass, "antigravity", runtimes.ModeSubprocessPerTurn, permission.ModeYolo},
		{PermissionModeDefault, "antigravity", runtimes.ModeSubprocessPerTurn, permission.ModeAcceptEdits},
		{PermissionModeBypass, "opencode", runtimes.ModeSubprocessPerTurn, ""},
		{PermissionModeDefault, "opencode", runtimes.ModeSubprocessPerTurn, ""},
		{PermissionModeBypass, "gemini", runtimes.ModeSubprocessPerTurn, ""},
		{PermissionModeBypass, "claude", runtimes.ModeACPStdio, ""},
		{PermissionModeBypass, "copilot", runtimes.ModeACPStdio, ""},
	}
	for _, tc := range tests {
		if got := ProviderPosture(tc.mode, tc.brand, tc.runtime); got != tc.want {
			t.Errorf("ProviderPosture(%q, %q, %q) = %q, want %q", tc.mode, tc.brand, tc.runtime, got, tc.want)
		}
	}
}

// Every posture Tether names must be one the provider's registry maps in
// each native mode it supports: agentkit fails a launch whose posture has no
// mapping.
func TestProviderPostureIsMappedByTheRegistry(t *testing.T) {
	for _, brand := range []string{"claude", "codex", "antigravity", "opencode"} {
		desc, ok := registry.Lookup(brand)
		if !ok {
			t.Fatalf("registry has no %q", brand)
		}
		for _, rt := range []runtimes.Mode{runtimes.ModePTY, runtimes.ModeStreamingStdio, runtimes.ModeJSONRPCStdio, runtimes.ModeSubprocessPerTurn} {
			if !desc.Supports(rt) {
				continue
			}
			for _, mode := range []string{PermissionModeBypass, PermissionModeDefault} {
				p := ProviderPosture(mode, brand, rt)
				if _, err := desc.PostureFor(p, rt); err != nil {
					t.Errorf("%s %s %s: posture %q: %v", brand, rt, mode, p, err)
				}
			}
		}
	}
}

// Default sessions retain the MCP approval hook; explicit bypass sessions
// disable the provider sandbox and approval prompts together.
func TestCodexPostureApprovalPolicy(t *testing.T) {
	desc, ok := registry.Lookup("codex")
	if !ok {
		t.Fatal("registry has no codex")
	}
	for _, rt := range []runtimes.Mode{runtimes.ModeSubprocessPerTurn, runtimes.ModeJSONRPCStdio} {
		for _, mode := range []string{PermissionModeBypass, PermissionModeDefault, ""} {
			launch, err := desc.PostureFor(ProviderPosture(mode, "codex", rt), rt)
			if err != nil {
				t.Fatalf("codex %s %q: %v", rt, mode, err)
			}
			want := `approval_policy="on-request"`
			if mode == PermissionModeBypass {
				want = `approval_policy="never"`
			}
			if !slices.Contains(launch.Args, want) {
				t.Errorf("codex %s %q: posture args %q lack %s", rt, mode, launch.Args, want)
			}
		}
	}
}
