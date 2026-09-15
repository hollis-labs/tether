package app

import (
	"context"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/launchprofile"
)

// ListProjects returns a flat slice of the catalog projects in iteration order.
func (s *Service) ListProjects() []config.Project {
	out := make([]config.Project, 0, len(s.Catalog.Projects))
	for _, p := range s.Catalog.Projects {
		out = append(out, p)
	}
	return out
}

// ListLaunchContexts returns a flat slice of the catalog launch contexts in iteration order.
func (s *Service) ListLaunchContexts() []config.LaunchContext {
	return s.ListProjects()
}

// ListAgents returns a flat slice of the catalog agents in iteration order.
func (s *Service) ListAgents() []config.Agent {
	out := make([]config.Agent, 0, len(s.Catalog.Agents))
	for _, a := range s.Catalog.Agents {
		out = append(out, a)
	}
	return out
}

// ListLaunchProfiles returns a flat slice of the catalog launch profiles in iteration order.
func (s *Service) ListLaunchProfiles() []config.LaunchProfile {
	return s.ListAgents()
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

func (s *Service) resolveWithInput(in CreateSessionInput) (*launch.Plan, error) {
	return launch.Resolve(s.Catalog, launch.Input{
		LaunchID:            in.LaunchID,
		CatalogRoot:         s.CatalogRoot,
		SkipPromptFragments: in.BootPromptOverride != "" || in.BootProfileFile != "",
	})
}

// ResolveComposition resolves a launch through the compositional launch resolution engine
// (internal/launchprofile), walking extends chains and folding project scope and launch inputs.
func (s *Service) ResolveComposition(ctx context.Context, in launchprofile.CompositionInput) (*launchprofile.ResolvedComposition, error) {
	resolver := launchprofile.NewResolver(s.Catalog)
	return resolver.Resolve(ctx, in)
}

// ResolveCompositionSnapshot produces an immutable, deterministic snapshot and SHA-256 digest
// for a compositional launch.
func (s *Service) ResolveCompositionSnapshot(ctx context.Context, in launchprofile.CompositionInput) (*launchprofile.Snapshot, error) {
	resolver := launchprofile.NewResolver(s.Catalog)
	return resolver.ResolveSnapshot(ctx, in)
}
