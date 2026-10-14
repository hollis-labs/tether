package app

import (
	"runtime"
	"testing"
)

// optInDarwinMCPProxyConfinement lets a test launch a protected Codex session
// on darwin. That launch confines the MCP proxy with the tether-mcp-control-plane
// sandbox profile, whose DenyUserServiceManager rule the darwin sandbox cannot
// enforce (launchd user agents), so LaunchSession refuses unless the operator
// opts in to the audited downgrade (AllowUnenforcedUserServiceManagerEnv).
func optInDarwinMCPProxyConfinement(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "darwin" {
		t.Setenv(AllowUnenforcedUserServiceManagerEnv, "1")
	}
}
