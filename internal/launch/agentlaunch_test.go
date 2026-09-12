package launch

import "testing"

// The approval hook in internal/app is only reachable when the PLANTED
// approval_policy lets codex ask. Under go-providers' "never" default it
// refuses every MCP tool call outright and the hook never runs, so this
// asserts the half of the fix that lives on the plan.
func TestProviderPermissionLetsCodexAsk(t *testing.T) {
	if got := providerPermission("codex"); got == "" || got == "never" {
		t.Fatalf("providerPermission(codex) = %q; must be a policy that elicits, or every MCP tool call is refused before the approval hook is consulted", got)
	}
}

// Claude's posture rides on argv, not on this field. Asserting the empty
// value keeps an incidental change to the planted settings.json visible.
func TestProviderPermissionLeavesClaudeAlone(t *testing.T) {
	if got := providerPermission("claude"); got != "" {
		t.Errorf("providerPermission(claude) = %q, want \"\"", got)
	}
}
