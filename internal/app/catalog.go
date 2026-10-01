package app

import (
	"context"
	"errors"
	"fmt"
	"log"

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
	cat, err := s.launchCatalog(launchID)
	if err != nil {
		return nil, err
	}
	return launch.Resolve(cat, launch.Input{LaunchID: launchID, CatalogRoot: s.CatalogRoot})
}

func (s *Service) resolveWithInput(in CreateSessionInput) (*launch.Plan, error) {
	cat, err := s.launchCatalog(in.LaunchID)
	if err != nil {
		return nil, err
	}
	return launch.Resolve(cat, launch.Input{
		LaunchID:            in.LaunchID,
		CatalogRoot:         s.CatalogRoot,
		SkipPromptFragments: in.BootPromptOverride != "" || in.BootProfileFile != "",
	})
}

// launchCatalog returns the catalog launchID resolves against: the startup
// catalog with its launches re-read from disk, so a launch file added or
// edited under launches/ takes effect without a daemon restart
// (CW-20261001-0018).
//
// Launches and upstream enablement are refreshed. Projects, agents, providers and sandbox
// profiles stay as loaded at startup, because session creation reads the
// agent's permission mode and sandbox profile from s.Catalog directly; a
// fresh agent here would reach that code with no sandbox entry to find. A
// launch that names something added after startup is refused by
// ValidateLaunch with an error that says so.
//
// A catalog that no longer loads (a YAML error anywhere in it) falls back
// to the startup launches, so one bad edit does not stop every launch. The
// Grant-validation errors always refuse; they never use stale grants. Other
// load errors are logged, and added to the not-found error when the startup
// launches do not have launchID either.
func (s *Service) launchCatalog(launchID string) (*config.Catalog, error) {
	if s.Catalog == nil || s.CatalogRoot == "" {
		return s.Catalog, nil
	}
	fresh, err := config.LoadLayered(s.CatalogRoot)
	if err != nil {
		if errors.Is(err, config.ErrInvalidMCPGrant) {
			return nil, err
		}
		log.Printf("app: launch %q: re-reading catalog launches failed, using the launches loaded at startup: %v", launchID, err)
		if _, ok := s.Catalog.Launches[launchID]; !ok {
			return nil, fmt.Errorf("launch %q %w in the launches loaded at startup, and re-reading the catalog failed: %w", launchID, launch.ErrLaunchNotFound, err)
		}
		return s.Catalog, nil
	}
	cat := *s.Catalog
	cat.Launches = fresh.Launches
	cat.MCPServerEnabled = fresh.MCPServerEnabled
	if _, ok := cat.Launches[launchID]; !ok {
		return &cat, nil // launch.Resolve reports the not-found
	}
	if err := cat.ValidateLaunch(launchID); err != nil {
		return nil, fmt.Errorf("%w (projects, agents and providers are loaded when the daemon starts; restart it to pick up new ones)", err)
	}
	return &cat, nil
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
