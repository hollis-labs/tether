package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hollis-labs/substrate/harness/agentlaunch"
	"github.com/hollis-labs/substrate/harness/agentlaunch/launcher"
	providerplant "github.com/hollis-labs/substrate/harness/agentlaunch/planting"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/launchartifacts"
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
		RuntimeKind:     string(compiled.Plan.Runtime),
		WorkspaceMode:   compiled.Plan.Workspace.Mode.String(),
		BootFile:        compiled.BootDirIntent.PerProviderBootFile,
		TransientFile:   compiled.BootDirIntent.TransientBootFile,
		MCPFile:         compiled.BootDirIntent.MCPDescriptorFile,
	}
}

func (s *Service) prepareSharedLaunch(ctx context.Context, plan *launch.Plan, workspaceDir string, plant plantContextInput) (result *agentlaunch.PreparedLaunch, err error) {
	if workspaceDir == "" {
		return nil, fmt.Errorf("workspace dir required")
	}
	var codexHome *launchartifacts.CodexHome
	if plan.ProviderBrand == "codex" {
		if plan.NativeResumeOnly {
			codexHome, err = launchartifacts.CaptureCodexHome(plan.NativeStateRoot)
		} else {
			codexHome, err = captureCodexHome()
		}
		if err != nil {
			return nil, err
		}
		defer func() {
			err = errors.Join(err, codexHome.Close())
			if err != nil {
				result = nil
			}
		}()
	}
	bootRoot := filepath.Join(workspaceDir, "boot")
	if err := os.MkdirAll(bootRoot, 0o750); err != nil {
		return nil, fmt.Errorf("create boot root: %w", err)
	}

	lp, err := s.agentLaunchPlanFor(ctx, plan, workspaceDir)
	if err != nil {
		return nil, err
	}
	if plan.NativeResumeOnly {
		// Capture preceded compilation; native state remains in the accepted source
		// home, while fresh policy artifacts retain their normal admission.
		if lp.Provider.Env == nil {
			lp.Provider.Env = map[string]string{}
		}
		lp.Provider.Env["CODEX_HOME"] = plan.NativeStateRoot
	}
	if plant.DaemonOwned {
		values := make([]string, 0, len(lp.Provider.Env))
		for key, value := range lp.Provider.Env {
			values = append(values, key+"="+value)
		}
		values, err = s.daemonWorkerEnv(values, launch.EffectiveMCPServers(plan.Env))
		if err != nil {
			return nil, err
		}
		lp.Provider.Env = map[string]string{}
		for _, value := range values {
			key, val, _ := strings.Cut(value, "=")
			lp.Provider.Env[key] = val
		}
	}
	lp.Workspace.TempPrefix = bootRoot
	compiled, err := launcher.Compile(ctx, lp, launcher.WithSourceCatalog(s.CatalogRoot, s.Catalog.Global.Version))
	if err != nil {
		return nil, err
	}
	storeSharedLaunchState(plan, compiled)
	prepared, custody, err := launchartifacts.Prepare(ctx, compiled, plant.ArtifactAdmission)
	if err != nil {
		return nil, err
	}
	defer func() {
		err = errors.Join(err, custody.Close())
		if err != nil {
			result = nil
		}
	}()
	// Use the neutral named-server contract: the provider library's self-MCP
	// helper fixes a legacy server key, while this gateway is named tether.
	if plant.TetherCommand != "" {
		prepared.PlantContext.MCPServers = append(prepared.PlantContext.MCPServers, agentlaunch.MCPServerSpec{
			Name: "tether", Command: plant.TetherCommand,
			Args: append([]string(nil), plant.TetherArgs...), Env: copyMap(plant.TetherEnv),
		})
	}
	if codexHome != nil {
		if err := custody.PlantCodex(ctx, prepared, codexHome, providerplant.WithResolver(plantResolver)); err != nil {
			return nil, err
		}
	} else {
		if err := providerplant.Plant(ctx, prepared, providerplant.WithResolver(plantResolver), providerplant.WithArtifactAuthorization(custody.Authorize)); err != nil {
			return nil, err
		}
	}
	return prepared, nil
}

type plantContextInput struct {
	ArtifactAdmission launchartifacts.Admission
	DaemonOwned       bool
	TetherCommand     string
	TetherArgs        []string
	TetherEnv         map[string]string
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
	lp, err := s.engineLaunchPlan(ctx, plan, workspaceDir)
	if err != nil {
		return agentlaunch.LaunchPlan{}, err
	}
	// Whichever engine produced the plan, an app-launched Claude loads only
	// the MCP servers Tether plants (CW-20261001-0227).
	s.applyClaudeStrictMCP(&lp)
	return lp, nil
}

// engineLaunchPlan is agentLaunchPlanFor's engine toggle: the plan as the
// selected launch engine produces it, before Tether's hardening is applied.
func (s *Service) engineLaunchPlan(ctx context.Context, plan *launch.Plan, workspaceDir string) (agentlaunch.LaunchPlan, error) {
	// Retained team recovery consumes its immutable launch receipt, rather than
	// resolving a changed catalog definition into existing enrollment authority.
	if (plan.TeamMember && plan.ResumeSourceSessionID != "") || s.launchEngine() != EngineSpec {
		return s.agentLaunchPlan(plan, workspaceDir), nil
	}
	resolver, err := s.specResolverFor()
	if err != nil {
		return agentlaunch.LaunchPlan{}, fmt.Errorf("spec launch engine: %w", err)
	}
	lp, err := resolver.ResolveContext(ctx, plan.LaunchID, agentlaunch.PolicyCollect)
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
