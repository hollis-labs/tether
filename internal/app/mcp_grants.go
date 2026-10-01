package app

import (
	"fmt"

	"github.com/hollis-labs/tether/internal/bootgen"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launch"
)

// validatePlanMCPGrants checks the actual grants after caller/agent overrides.
// Read the current upstream catalog, as the proxy does, rather than trusting
// the daemon's startup enablement snapshot. This never resolves credentials.
func (s *Service) validatePlanMCPGrants(plan *launch.Plan, profile bootgen.Profile, profilePath string) error {
	value, explicit := plan.Env[launch.MCPServersEnv]
	if profile.MCPServers == nil && !explicit {
		return nil
	}
	cat := s.Catalog
	if s.CatalogRoot != "" {
		entries, err := config.LoadMCPServerCatalog(s.CatalogRoot)
		if err != nil {
			return fmt.Errorf("validate MCP grants: %w", err)
		}
		cat = &config.Catalog{MCPServerEnabled: make(map[string]bool, len(entries))}
		for _, entry := range entries {
			cat.MCPServerEnabled[entry.ID] = entry.IsEnabled()
		}
	}
	if err := cat.ValidateMCPGrant("boot profile "+profilePath, profile.MCPServers); err != nil {
		return err
	}
	if explicit {
		return cat.ValidateMCPGrantEnv(fmt.Sprintf("launch %q agent %q", plan.LaunchID, plan.LogicalAgentID), value)
	}
	return nil
}
