package app

import (
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/mcpgateway"
)

// sessionMCPPolicy runs after provider/boot overrides have been resolved into
// the persisted plan. Copy only authority selectors, never the plan's secrets.
func sessionMCPPolicy(id, agent string, plan *launch.Plan) mcpgateway.SessionPolicy {
	policy := mcpgateway.SessionPolicy{SessionID: id, AgentID: agent, Servers: launch.EffectiveMCPServers(plan.Env)}
	if value, set := plan.Env["TETHER_MCP_PROFILE"]; set {
		policy.Profile = &value
	}
	if value, set := plan.Env["TETHER_MCP_DISCOVERY_MODE"]; set {
		policy.DiscoveryMode = &value
	}
	return policy.Seal()
}
