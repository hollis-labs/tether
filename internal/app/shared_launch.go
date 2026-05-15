package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hollis-labs/go-agent-launch/agentlaunch"
	"github.com/hollis-labs/go-agent-launch/agentlaunch/launcher"
	"github.com/hollis-labs/go-agent-launch/agentlaunch/providerplant"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launch"
)

func (s *Service) compileSharedLaunch(ctx context.Context, plan *launch.Plan) error {
	if plan == nil || plan.ProviderBrand == "api-stub" || plan.RuntimeKind == config.RuntimeKindAPI {
		return nil
	}
	compiled, err := launcher.Compile(ctx, s.agentLaunchPlan(plan, plan.WriteHome), launcher.WithSourceCatalog(s.CatalogRoot, s.Catalog.Global.Version))
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

	lp := s.agentLaunchPlan(plan, workspaceDir)
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
	prepared.PlantContext.MuxCommand = plant.MuxCommand
	prepared.PlantContext.MuxArgs = append([]string(nil), plant.MuxArgs...)
	prepared.PlantContext.MuxEnv = copyMap(plant.MuxEnv)
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

func (s *Service) agentLaunchPlan(plan *launch.Plan, workspaceDir string) agentlaunch.LaunchPlan {
	return agentlaunch.LaunchPlan{
		Project: agentlaunch.ProjectSpec{
			ID:   plan.ProjectID,
			Root: plan.RepoRoot,
		},
		Agent: agentlaunch.AgentSpec{
			ID: plan.LogicalAgentID,
		},
		Provider: agentlaunch.ProviderSpec{
			ID:     plan.ProviderBrand,
			Binary: plan.Command,
			Flags:  append([]string(nil), plan.Args...),
			Env:    copyMap(plan.Env),
		},
		Runtime:   mapSharedRuntime(plan.RuntimeKind),
		Workspace: mapSharedWorkspace(plan, workspaceDir),
		BootProfile: agentlaunch.BootProfileRef{
			Inline: &agentlaunch.BootProfileInline{
				BootPrompt: plan.BootPrompt,
				BootMode:   mapSharedBootMode(plan.BootMode),
			},
		},
		MCP: agentlaunch.MCPSpec{
			Allowlist: splitCSV(plan.Env["MUX_MCP_SERVERS"]),
		},
		Injection: agentlaunch.InjectionSpec{
			NativeFiles:    nativeFilesForAgentLaunch(plan.NativeFiles),
			BootDirOverlay: copyMap(plan.BootDirOverlay),
		},
		Mode: agentlaunch.LaunchInteractive,
		Metadata: agentlaunch.Metadata{
			Annotations: map[string]string{
				"tether.launch_id":      plan.LaunchID,
				"tether.provider_id":    plan.ProviderID,
				"tether.workspace_mode": plan.WorkspaceMode,
			},
		},
	}
}

func mapSharedRuntime(runtime string) agentlaunch.RuntimeKind {
	switch runtime {
	case config.RuntimeKindPTY:
		return agentlaunch.RuntimePTY
	case config.RuntimeKindStreamingStdio:
		return agentlaunch.RuntimeStreamingStdio
	case config.RuntimeKindJSONRPCStdio:
		return agentlaunch.RuntimeJsonRpcStdio
	default:
		return agentlaunch.RuntimeSubprocess
	}
}

func mapSharedWorkspace(plan *launch.Plan, workspaceDir string) agentlaunch.WorkspaceSpec {
	mode := agentlaunch.WorkspacePersistent
	switch plan.WorkspaceMode {
	case "shared", "hybrid", "":
		mode = agentlaunch.WorkspacePersistent
	case "worktree", "isolated":
		mode = agentlaunch.WorkspacePersistent
	case "temp":
		mode = agentlaunch.WorkspaceTemp
	case "fresh":
		mode = agentlaunch.WorkspaceFresh
	}
	return agentlaunch.WorkspaceSpec{
		Mode:         mode,
		Workdir:      plan.EffectiveWorkRoot(),
		WorkspaceDir: workspaceDir,
	}
}

func mapSharedBootMode(mode string) string {
	switch mode {
	case agentlaunch.BootModeNone, agentlaunch.BootModeStdin, agentlaunch.BootModePlanted:
		return mode
	default:
		return agentlaunch.BootModePlanted
	}
}

func nativeFilesForAgentLaunch(in []launch.NativeFile) []agentlaunch.NativeFile {
	if len(in) == 0 {
		return nil
	}
	out := make([]agentlaunch.NativeFile, 0, len(in))
	for _, f := range in {
		out = append(out, agentlaunch.NativeFile{
			Kind:    agentlaunch.NativeFileKind(f.Kind),
			ID:      f.ID,
			RelPath: filepath.ToSlash(f.RelPath),
			Content: f.Content,
			Mode:    os.FileMode(f.Mode),
		})
	}
	return out
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

func splitCSV(in string) []string {
	var out []string
	for _, part := range strings.Split(in, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}
