package app

import (
	"fmt"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/mcpgateway"
)

// sessionMCPPolicy runs after provider/boot overrides have been resolved into
// the persisted plan. Copy only authority selectors, never the plan's secrets.
func sessionMCPPolicy(id, agent string, plan *launch.Plan, cfg mcpgateway.Config) (mcpgateway.SessionPolicy, error) {
	policy := mcpgateway.SessionPolicy{SessionID: id, AgentID: agent, Servers: launch.EffectiveMCPServers(plan.Env)}
	if value, set := plan.Env["TETHER_MCP_PROFILE"]; set {
		policy.Profile = &value
		selection, err := mcpgateway.ResolveProfile(cfg, mcpgateway.ProfileInputs{Environment: &value})
		if err != nil {
			return policy, fmt.Errorf("%w: %w", mcpgateway.ErrInvalidSessionMCPPolicy, err)
		}
		policy.LaunchProfile = selection.Profile
	}
	if value, set := plan.Env["TETHER_MCP_DISCOVERY_MODE"]; set {
		policy.DiscoveryMode = &value
	}
	policy = policy.Seal()
	return policy, policy.Validate()
}
