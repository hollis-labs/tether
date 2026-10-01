package launchresolve

import (
	"testing"

	permission "github.com/hollis-labs/go-permission"
)

// TestResolveRuntimeBinding_Permission verifies the spec engine threads the
// same posture as the catalog engine (config.ProviderPosture) onto a resolved
// binding, from the fixture catalog's global.yaml
// defaults.permission_mode: bypass.
func TestResolveRuntimeBinding_Permission(t *testing.T) {
	reg := openFixture(t)

	claude, err := reg.ResolveRuntimeBinding("claude-stream")
	if err != nil {
		t.Fatalf("resolve claude-stream: %v", err)
	}
	if claude.Permission != permission.ModeYolo {
		t.Errorf("claude binding Permission = %q, want %q", claude.Permission, permission.ModeYolo)
	}

	codex, err := reg.ResolveRuntimeBinding("codex-cli")
	if err != nil {
		t.Fatalf("resolve codex-cli: %v", err)
	}
	if codex.Permission != permission.ModeAcceptEdits {
		t.Errorf("codex binding Permission = %q, want %q (workspace-write + on-request, so MCP tool calls can be approved)", codex.Permission, permission.ModeAcceptEdits)
	}
}
