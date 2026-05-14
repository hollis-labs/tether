package app

import (
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launch"
)

// ListProjects returns a flat slice of the catalog projects in iteration order.
func (s *Service) ListProjects() []config.Project {
	out := make([]config.Project, 0, len(s.Catalog.Projects))
	for _, p := range s.Catalog.Projects {
		out = append(out, p)
	}
	return out
}

// ListAgents returns a flat slice of the catalog agents in iteration order.
func (s *Service) ListAgents() []config.Agent {
	out := make([]config.Agent, 0, len(s.Catalog.Agents))
	for _, a := range s.Catalog.Agents {
		out = append(out, a)
	}
	return out
}

// ListProviders returns a flat slice of the catalog providers in iteration order.
func (s *Service) ListProviders() []config.Provider {
	out := make([]config.Provider, 0, len(s.Catalog.Providers))
	for _, p := range s.Catalog.Providers {
		out = append(out, p)
	}
	return out
}

// Resolve assembles a launch.Plan for the given launch profile id by walking
// the layered catalog (project + agent + provider + sandbox profile + boot
// fragments). See internal/launch for the merge order.
func (s *Service) Resolve(launchID string) (*launch.Plan, error) {
	return launch.Resolve(s.Catalog, launch.Input{LaunchID: launchID, CatalogRoot: s.CatalogRoot})
}
