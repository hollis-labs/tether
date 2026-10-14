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

// MachineChangingMCPServers are upstreams that can change machines (ssh,
// resources, enrollment, installs). Only agents with machine_authority may be
// granted them; placement on a machine needs no such grant.
var MachineChangingMCPServers = []string{"cerberus"}

// MachineGrantViolations returns the machine-changing upstreams in granted.
func MachineGrantViolations(granted []string) []string {
	var out []string
	for _, id := range granted {
		for _, m := range MachineChangingMCPServers {
			if id == m {
				out = append(out, id)
			}
		}
	}
	return out
}

// MachineAccessEnv carries the agent's designated machine access ("read-only"
// or "read-write") to the proxy, which enforces it on every call to a
// MachineChangingMCPServers upstream. Absent or unknown means read-only.
const MachineAccessEnv = "TETHER_MACHINE_ACCESS"

// MachineDevModeEnv set to "1" in the proxy env grants full machine access
// while dev mode is on; every call is still audited.
const MachineDevModeEnv = "TETHER_DEV_MODE"

// Machine access values for MachineAccessEnv and the agent's machine_access.
const (
	MachineAccessReadOnly  = "read-only"
	MachineAccessReadWrite = "read-write"
)
