package app

import (
	"fmt"
	"os/exec"

	"github.com/hollis-labs/substrate/harness/sandbox"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launch"
)

// ConfineMCPPlant protects the whole local MCP process tree, rather than
// trusting individual tools to reject writes. Codex spawns its MCP servers
// outside its own sandbox; wrapping only its proxy avoids nesting bubblewrap
// around Codex itself or around Claude's already-wrapped proxy. A failed
// wrapper never falls back to running the proxy on the host.
//
// This is control-plane write protection, not workspace confinement: upstreams
// retain host reads, network access and writes outside the protected trees.
// HTTP/SSE servers cannot inherit this namespace and are refused separately
// by a protected proxy. The Codex config/caller-identity guard remains dormant.
func ConfineMCPPlant(plan *launch.Plan, command string, args, protected []string) (string, []string, error) {
	if plan == nil || plan.ProviderBrand != "codex" || len(protected) == 0 {
		return command, args, nil
	}
	// #nosec G204 -- daemon-selected proxy executable and planted arguments.
	cmd := exec.Command(command, args...)
	profile := sandbox.Profile{
		ID: "tether-mcp-control-plane", HostFilesystem: true,
		Net: true, Subprocess: true, DenyUserServiceManager: true,
		FS: sandbox.FSSpec{Protect: protected},
	}
	cleanup, err := sandbox.Apply(cmd, profile, plan.RepoRoot)
	if err != nil {
		return "", nil, fmt.Errorf("confine Codex MCP proxy: %w", err)
	}
	cleanup() // protect-only has no runtime helper or temporary resources
	return cmd.Path, cmd.Args[1:], nil
}

func confinedMCPEnv(plan *launch.Plan, protected []string) map[string]string {
	env := tetherEnvMap(plan.Env)
	if plan.ProviderBrand == "codex" && len(protected) > 0 {
		env[config.MCPConfineRemoteEnv] = "1"
	}
	return env
}
