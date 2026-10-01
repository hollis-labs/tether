package launch

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/hollis-labs/agent-contracts-leaf/runtimes"
	"github.com/hollis-labs/agentkit/agentlaunch"

	"github.com/hollis-labs/tether/internal/config"
)

// AgentLaunchPlan maps Tether's persisted launch.Plan onto the shared
// go-agent-launch LaunchPlan contract. Tether still owns catalog resolution,
// prompt composition, environment policy, and workspace materialization; this
// function owns only the app-neutral contract translation.
func AgentLaunchPlan(plan *Plan, workspaceDir string) agentlaunch.LaunchPlan {
	projectID := plan.ProjectID
	if projectID == "" {
		projectID = "project"
	}
	agentID := AgentName(plan)
	runtime := mapRuntime(plan.RuntimeKind)
	return agentlaunch.LaunchPlan{
		Project: agentlaunch.ProjectSpec{
			ID:   projectID,
			Root: plan.RepoRoot,
		},
		Agent: agentlaunch.AgentSpec{
			ID:   agentID,
			Name: plantedAgentName(plan),
		},
		Provider: agentlaunch.ProviderSpec{
			ID:         plan.ProviderBrand,
			Binary:     plan.Command,
			Flags:      CatalogFlags(plan),
			Env:        copyMap(plan.Env),
			Permission: config.ProviderPosture(plan.PermissionMode, plan.ProviderBrand, runtime),
		},
		Runtime: runtime,
		Workspace: agentlaunch.WorkspaceSpec{
			Mode:         mapWorkspaceMode(plan.WorkspaceMode),
			Workdir:      plan.EffectiveWorkRoot(),
			WorkspaceDir: workspaceDir,
		},
		BootProfile: agentlaunch.BootProfileRef{
			Inline: &agentlaunch.BootProfileInline{
				BootPrompt: plan.BootPrompt,
				BootMode:   mapBootMode(plan.BootMode),
			},
		},
		MCP: agentlaunch.MCPSpec{
			Allowlist: splitCSV(plan.Env["TETHER_MCP_SERVERS"]),
		},
		Injection: agentlaunch.InjectionSpec{
			NativeFiles:    nativeFiles(plan.NativeFiles),
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

// AgentName is the agent id the shared launch is planned under, and so the
// name the providers' planted agent files take (opencode's
// agents/<name>.md, selected with --agent <name>): the plan's logical agent,
// else "agent".
func AgentName(plan *Plan) string {
	if plan.LogicalAgentID != "" {
		return plan.LogicalAgentID
	}
	return "agent"
}

// OpencodeAgentName is the name of the agent file the shared launch plants
// for opencode (agents/<name>.md) and selects with --agent. It is namespaced
// because opencode merges a planted agent file into its built-in agent of the
// same name: a Tether agent called general, plan or explore would otherwise
// run as opencode's own agent of that name with its mode and permissions
// (plan is edit-deny, explore denies everything).
func OpencodeAgentName(plan *Plan) string {
	return "tether-" + AgentName(plan)
}

// plantedAgentName is the agent display name the providers' planters use
// (agentkit takes AgentSpec.Name before ID). Only opencode turns it into a
// file and a flag that can collide, so only opencode's is namespaced; the
// other providers keep the agent id.
func plantedAgentName(plan *Plan) string {
	if plan.ProviderBrand == "opencode" {
		return OpencodeAgentName(plan)
	}
	return ""
}

// CatalogFlags is plan.Args less what the provider's own argv convention
// already emits; see config.CatalogFlags.
func CatalogFlags(plan *Plan) []string {
	return config.CatalogFlags(plan.ProviderBrand, plan.Args)
}

// mapRuntime maps a plan's runtime-kind token onto the shared plan's mode.
// Only the modes Tether launches pass through (the four native ones and
// acp-stdio); anything else (api, serve-http, pty-debug, unknown) falls back
// to subprocess-per-turn, as it did before the leaf vocabulary.
func mapRuntime(runtime string) runtimes.Mode {
	mode, debug, ok := config.RuntimeMode(runtime)
	if !ok || debug {
		return runtimes.ModeSubprocessPerTurn
	}
	switch mode {
	case runtimes.ModePTY, runtimes.ModeStreamingStdio, runtimes.ModeJSONRPCStdio, runtimes.ModeSubprocessPerTurn, runtimes.ModeACPStdio:
		return mode
	default:
		return runtimes.ModeSubprocessPerTurn
	}
}

func mapWorkspaceMode(mode string) agentlaunch.WorkspaceMode {
	switch mode {
	case "shared", "hybrid", "":
		return agentlaunch.WorkspacePersistent
	case "worktree", "isolated":
		return agentlaunch.WorkspacePersistent
	case "temp":
		return agentlaunch.WorkspaceTemp
	case "fresh":
		return agentlaunch.WorkspaceFresh
	default:
		return agentlaunch.WorkspacePersistent
	}
}

func mapBootMode(mode string) string {
	switch mode {
	case agentlaunch.BootModeNone, agentlaunch.BootModeStdin, agentlaunch.BootModePlanted:
		return mode
	default:
		return agentlaunch.BootModePlanted
	}
}

func nativeFiles(in []NativeFile) []agentlaunch.NativeFile {
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
