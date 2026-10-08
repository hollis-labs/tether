package launch

import "strings"

// MCPServersEnv is the plan.Env key that carries the planted proxy's upstream
// allow-list: the comma-separated server ids a launched agent's `tether mcp
// --proxy` is granted. It is set from a launch's or project's mcp.servers or a
// boot profile's mcp_servers.
const MCPServersEnv = "TETHER_MCP_SERVERS"

// DefaultMCPServers grants the task tracker only (CW-20261001-0228,
// Chrispian's interim policy). Tesseract and every other upstream are opt-in
// per launch, project or boot profile until the daemon-side credential fix lands.
// An explicit list replaces this default.
var DefaultMCPServers = []string{"torque"}

// EffectiveMCPServers is the allow-list a launched agent's proxy runs with:
// the plan's own list (including empty), or DefaultMCPServers when unset.
func EffectiveMCPServers(env map[string]string) []string {
	if _, set := env[MCPServersEnv]; !set {
		return append([]string(nil), DefaultMCPServers...)
	}
	var out []string
	for _, id := range strings.Split(env[MCPServersEnv], ",") {
		if id = strings.TrimSpace(id); id != "" {
			out = append(out, id)
		}
	}
	return out
}
