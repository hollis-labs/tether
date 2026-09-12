package launch

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/hollis-labs/agentkit/agentlaunch"
	"github.com/hollis-labs/agentkit/agentruntime/runtimekind"
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
	agentID := plan.LogicalAgentID
	if agentID == "" {
		agentID = "agent"
	}
	return agentlaunch.LaunchPlan{
		Project: agentlaunch.ProjectSpec{
			ID:   projectID,
			Root: plan.RepoRoot,
		},
		Agent: agentlaunch.AgentSpec{
			ID: agentID,
		},
		Provider: agentlaunch.ProviderSpec{
			ID:         plan.ProviderBrand,
			Binary:     plan.Command,
			Flags:      append([]string(nil), plan.Args...),
			Env:        copyMap(plan.Env),
			Permission: providerPermission(plan.ProviderBrand),
		},
		Runtime: mapRuntime(plan.RuntimeKind),
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
			Allowlist: splitCSV(plan.Env["MUX_MCP_SERVERS"]),
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

func mapRuntime(runtime string) agentlaunch.RuntimeKind {
	switch k := runtimekind.Parse(runtime); k {
	case runtimekind.PTY, runtimekind.StreamingStdio, runtimekind.JSONRPCStdio, runtimekind.Subprocess:
		return k
	default:
		return runtimekind.Subprocess
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

// providerPermission maps Tether's launch posture onto the approval
// vocabulary the planting adapter expects. providerplant.DefaultResolver
// assigns ProviderSpec.Permission straight onto the adapter
// (ClaudeAdapter.PermissionMode / CodexAdapter.ApprovalPolicy), and each
// provider's vocabulary is its own — there is no shared one.
//
// Tether left this empty for every provider, which for codex meant
// go-providers' "never" default, and under "never" codex refuses EVERY MCP
// tool call before it even asks ("MCP tool call requires approval, but
// approval policy is never"). A launched codex worker could see the mux MCP
// server its own launch planted and call nothing on it.
//
// "on-request" is the value that lets codex ask instead of refusing;
// internal/app/codex_approval.go answers those requests. Both halves are
// required — the policy alone just moves the failure, and the hook alone is
// never reached.
//
// Only codex is mapped here. Claude is deliberately left empty: its headless
// posture already rides on the --dangerously-skip-permissions flag that
// launch.Resolve splices into argv, and changing what lands in the planted
// settings.json is a separate change with its own blast radius.
func providerPermission(providerBrand string) string {
	if providerBrand == "codex" {
		return "on-request"
	}
	return ""
}
