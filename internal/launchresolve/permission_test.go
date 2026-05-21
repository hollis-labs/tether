package launchresolve

import "testing"

// TestClaudePermission covers the Tether permission_mode -> claude
// permission-vocabulary mapping. The mapping must always return a
// non-empty value: an empty Permission on a claude runtime-binding leaves
// a headless launch hanging on the first approval prompt.
func TestClaudePermission(t *testing.T) {
	tests := []struct {
		permissionMode string
		want           string
	}{
		{"bypass", "bypassPermissions"},
		{"default", "default"},
		{"", "default"},
		{"nonsense", "default"},
	}
	for _, tc := range tests {
		if got := claudePermission(tc.permissionMode); got != tc.want {
			t.Errorf("claudePermission(%q) = %q, want %q", tc.permissionMode, got, tc.want)
		}
		if claudePermission(tc.permissionMode) == "" {
			t.Errorf("claudePermission(%q) returned empty — a claude binding must never carry empty Permission", tc.permissionMode)
		}
	}
}

// TestResolveRuntimeBinding_Permission verifies the permission posture is
// threaded onto a resolved claude binding (from the fixture catalog's
// global.yaml defaults.permission_mode: bypass) and left empty for codex
// (go-providers defaults codex's approval_policy to "never").
func TestResolveRuntimeBinding_Permission(t *testing.T) {
	reg := openFixture(t)

	claude, err := reg.ResolveRuntimeBinding("claude-stream")
	if err != nil {
		t.Fatalf("resolve claude-stream: %v", err)
	}
	if claude.Permission != "bypassPermissions" {
		t.Errorf("claude binding Permission = %q, want %q", claude.Permission, "bypassPermissions")
	}

	codex, err := reg.ResolveRuntimeBinding("codex-cli")
	if err != nil {
		t.Fatalf("resolve codex-cli: %v", err)
	}
	if codex.Permission != "" {
		t.Errorf("codex binding Permission = %q, want empty (go-providers defaults approval_policy to never)", codex.Permission)
	}
}
