package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/hollis-labs/agentkit/agentlaunch"
	"github.com/hollis-labs/agentkit/agentlaunch/launcher"
	"github.com/hollis-labs/agentkit/agentlaunch/providerplant"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launch"
)

func (s *Service) compileSharedLaunch(ctx context.Context, plan *launch.Plan) error {
	if plan == nil || plan.ProviderBrand == "api-stub" || plan.RuntimeKind == config.RuntimeKindAPI {
		return nil
	}
	lp, err := s.agentLaunchPlanFor(ctx, plan, plan.WriteHome)
	if err != nil {
		return err
	}
	compiled, err := launcher.Compile(ctx, lp, launcher.WithSourceCatalog(s.CatalogRoot, s.Catalog.Global.Version))
	if err != nil {
		return err
	}
	storeSharedLaunchState(plan, compiled)
	return nil
}

func storeSharedLaunchState(plan *launch.Plan, compiled *agentlaunch.CompiledLaunch) {
	if plan == nil || compiled == nil {
		return
	}
	plan.Shared = &launch.SharedLaunchState{
		PlanHash:        compiled.Provenance.PlanHash,
		CompilerVersion: compiled.Provenance.CompilerVersion,
		ProviderID:      compiled.Plan.Provider.ID,
		RuntimeKind:     compiled.Plan.Runtime.String(),
		WorkspaceMode:   compiled.Plan.Workspace.Mode.String(),
		BootFile:        compiled.BootDirIntent.PerProviderBootFile,
		TransientFile:   compiled.BootDirIntent.TransientBootFile,
		MCPFile:         compiled.BootDirIntent.MCPDescriptorFile,
	}
}

func (s *Service) prepareSharedLaunch(ctx context.Context, plan *launch.Plan, workspaceDir string, plant plantContextInput) (*agentlaunch.PreparedLaunch, error) {
	if workspaceDir == "" {
		return nil, fmt.Errorf("workspace dir required")
	}
	bootRoot := filepath.Join(workspaceDir, "boot")
	if err := os.MkdirAll(bootRoot, 0o750); err != nil {
		return nil, fmt.Errorf("create boot root: %w", err)
	}

	lp, err := s.agentLaunchPlanFor(ctx, plan, workspaceDir)
	if err != nil {
		return nil, err
	}
	lp.Workspace.TempPrefix = bootRoot
	compiled, err := launcher.Compile(ctx, lp, launcher.WithSourceCatalog(s.CatalogRoot, s.Catalog.Global.Version))
	if err != nil {
		return nil, err
	}
	storeSharedLaunchState(plan, compiled)
	prepared, err := launcher.Prepare(ctx, compiled)
	if err != nil {
		return nil, err
	}
	prepared.PlantContext.SelfMCPCommand = plant.MuxCommand
	prepared.PlantContext.SelfMCPArgs = append([]string(nil), plant.MuxArgs...)
	prepared.PlantContext.SelfMCPEnv = copyMap(plant.MuxEnv)
	if err := providerplant.Plant(ctx, prepared); err != nil {
		return nil, err
	}
	return prepared, nil
}

type plantContextInput struct {
	MuxCommand string
	MuxArgs    []string
	MuxEnv     map[string]string
}

// agentLaunchPlanFor produces the agentlaunch.LaunchPlan that feeds
// launcher.Compile. It is the single S5 toggle branch point.
//
//   - EngineCatalog (default): the plan is built from the daemon's already-
//     resolved launch.Plan via agentLaunchPlan — byte-identical to the
//     pre-S5 path.
//   - EngineSpec: the plan is resolved from plan.LaunchID by the S5
//     LaunchSpec resolver (internal/specresolve). The daemon's own
//     launch.Resolve still runs upstream for launch.Plan bookkeeping
//     (persistence/rehydration/provenance) — that transitional double-
//     resolve is intended for the soak. Only the workspace dir, which the
//     daemon owns, is folded back onto the resolver's plan.
//
// The toggle changes only HOW this plan is produced; launch.Plan and every
// path downstream of Compile are untouched.
func (s *Service) agentLaunchPlanFor(ctx context.Context, plan *launch.Plan, workspaceDir string) (agentlaunch.LaunchPlan, error) {
	if s.launchEngine() != EngineSpec {
		return s.agentLaunchPlan(plan, workspaceDir), nil
	}
	resolver, err := s.specResolverFor()
	if err != nil {
		return agentlaunch.LaunchPlan{}, fmt.Errorf("spec launch engine: %w", err)
	}
	lp, err := resolver.ResolveContext(ctx, plan.LaunchID, agentlaunch.FrontEndInteractive)
	if err != nil {
		return agentlaunch.LaunchPlan{}, fmt.Errorf("spec launch engine: resolve %q: %w", plan.LaunchID, err)
	}
	// The daemon owns the materialized workspace directory; the Spec
	// resolver does not know it. Fold it onto the resolved plan so the
	// boot-dir layout plants into the daemon's workspace.
	lp.Workspace.WorkspaceDir = workspaceDir
	return lp, nil
}

func (s *Service) agentLaunchPlan(plan *launch.Plan, workspaceDir string) agentlaunch.LaunchPlan {
	return launch.AgentLaunchPlan(plan, workspaceDir)
}

func copyMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
