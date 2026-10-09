package mcptransport

import (
	"github.com/hollis-labs/tether/internal/mcpadapter"
	"github.com/hollis-labs/tether/internal/mcpgateway"
)

// ViewOptions translates selectors only after resolving credential authority.
// Environment defaults come from the stored session snapshot, not daemon env
// or a caller-supplied session/scopes/servers flag. The builder intersects the
// selected profile with this explicit credential grant in both modes.
func ViewOptions(caller Caller, cfg mcpgateway.Config, profiles, modes []mcpgateway.Selector) (mcpadapter.ProxyOptions, error) {
	floors := []mcpgateway.ProfileSelection{}
	if caller.Policy.ToolProfile != nil {
		floor := mcpgateway.CloneProfile(*caller.Policy.ToolProfile)
		floors = append(floors, mcpgateway.ProfileSelection{Source: "boot tools", Profile: &floor})
	}
	if caller.Policy.Profile != nil {
		if caller.Policy.LaunchProfile == nil {
			return mcpadapter.ProxyOptions{}, mcpgateway.ErrInvalidSessionMCPPolicy
		}
		floor := mcpgateway.CloneProfile(*caller.Policy.LaunchProfile)
		floors = append(floors, mcpgateway.ProfileSelection{ID: *caller.Policy.Profile, Source: "launch", Profile: &floor})
	}
	profile, err := mcpgateway.ResolveProfile(cfg, mcpgateway.ProfileInputs{Explicit: profiles, Environment: caller.Policy.Profile})
	if err != nil {
		return mcpadapter.ProxyOptions{}, err
	}
	inputs := mcpgateway.ModeInputs{Explicit: modes, Environment: caller.Policy.DiscoveryMode, Profile: profile.Profile, Gateway: cfg.DiscoveryMode}
	if _, err := mcpgateway.ResolveMode(inputs); err != nil {
		return mcpadapter.ProxyOptions{}, err
	}
	return mcpadapter.ProxyOptions{ServerFilter: append([]string{}, caller.Policy.Servers...), Confine: true, Profile: profile, AuthorityProfiles: floors, ModeInputs: inputs}, nil
}
