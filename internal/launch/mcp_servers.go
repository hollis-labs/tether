package launch

import "strings"

// MCPServersEnv is the plan.Env key that carries the planted proxy's upstream
// allow-list: the comma-separated server ids a launched agent's `mux mcp
// --proxy` is granted. It is set from a launch's or project's mcp.servers or a
// boot profile's mcp_servers.
const MCPServersEnv = "MUX_MCP_SERVERS"

// DefaultMCPServers is the allow-list a launched agent's proxy gets when
// nothing sets one (CW-20261001-0227, approved by lead): the task tracker and
// the memory store. Every other upstream -- cerberus (infrastructure control),
// nanite (database access), hadron, loom, sigil, fragments-engine -- is opt-in,
// per launch, project or boot profile, and cerberus is never on by default.
//
// An explicit list REPLACES this default; it does not add to it. A launch that
// lists [loom] gets loom and neither of these.
var DefaultMCPServers = []string{"torque", "tesseract"}

// EffectiveMCPServers is the allow-list a launched agent's proxy runs with:
// the plan's own list, or DefaultMCPServers when it has none.
func EffectiveMCPServers(env map[string]string) []string {
	var out []string
	for _, id := range strings.Split(env[MCPServersEnv], ",") {
		if id = strings.TrimSpace(id); id != "" {
			out = append(out, id)
		}
	}
	if len(out) > 0 {
		return out
	}
	return append([]string(nil), DefaultMCPServers...)
}
