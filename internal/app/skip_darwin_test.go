package app

import (
	"runtime"
	"testing"
)

// skipWithoutMCPProxyConfinement skips tests that launch a protected Codex
// session. That launch confines the MCP proxy with the
// tether-mcp-control-plane sandbox profile, whose DenyUserServiceManager rule
// the darwin sandbox cannot enforce (launchd user agents), so LaunchSession
// refuses by design. Production behavior is unchanged; only the test is skipped.
func skipWithoutMCPProxyConfinement(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "darwin" {
		t.Skip("DenyUserServiceManager is not enforced on darwin; protected Codex launch is refused by design")
	}
}
