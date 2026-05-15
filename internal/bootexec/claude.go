package bootexec

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hollis-labs/go-agent-launch/agentlaunch"
	"github.com/hollis-labs/go-agent-launch/agentlaunch/launcher"
	"github.com/hollis-labs/go-agent-launch/agentlaunch/providerplant"
	gop "github.com/hollis-labs/go-providers/provider"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launch"
	tetherprovider "github.com/hollis-labs/tether/internal/provider"
)

// Options carries direct-exec-only inputs that are normally supplied by the
// agentsessions StartOptions path.
type Options struct {
	BootDirRoot      string
	APIKeyHelperPath string
	MuxCommand       string
	MuxArgs          []string
	MuxEnv           []string
	ParentEnv        []string
}

// Prepared is the fully materialized direct CLI invocation.
type Prepared struct {
	Command      string
	Args         []string
	Dir          string
	Env          []string
	BootDir      string
	WorkspaceDir string
}

// PrepareClaudeTUI materializes the Claude boot-dir layout and returns a
// command line that runs the real Claude TUI in the caller's terminal.
func PrepareClaudeTUI(plan *launch.Plan, opts Options) (*Prepared, error) {
	if plan == nil {
		return nil, fmt.Errorf("launch plan required")
	}
	if plan.ProviderBrand != "claude" {
		return nil, fmt.Errorf("boot-exec currently supports claude launch profiles only (got provider %q)", plan.ProviderBrand)
	}

	root := opts.BootDirRoot
	if root == "" {
		root = os.TempDir()
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, fmt.Errorf("ensure boot-exec root %s: %w", root, err)
	}
	workspaceDir, err := os.MkdirTemp(root, "tether-boot-exec-workspace-*")
	if err != nil {
		return nil, fmt.Errorf("create boot-exec workspace: %w", err)
	}

	lp := agentLaunchPlan(plan, workspaceDir)
	lp.Workspace.TempPrefix = root
	compiled, err := launcher.Compile(context.Background(), lp)
	if err != nil {
		_ = os.RemoveAll(workspaceDir)
		return nil, err
	}
	prepared, err := launcher.Prepare(context.Background(), compiled)
	if err != nil {
		_ = os.RemoveAll(workspaceDir)
		return nil, err
	}
	prepared.PlantContext.MuxCommand = opts.MuxCommand
	prepared.PlantContext.MuxArgs = append([]string(nil), opts.MuxArgs...)
	prepared.PlantContext.MuxEnv = muxEnvMap(opts.MuxEnv)

	adapter := gop.NewClaudeAdapterPTY()
	adapter.ApiKeyHelperPath = opts.APIKeyHelperPath
	if err := providerplant.Plant(context.Background(), prepared, providerplant.WithAdapter(adapter)); err != nil {
		_ = os.RemoveAll(workspaceDir)
		return nil, err
	}

	env := tetherprovider.BuildEnv(plan.EnvMode, plan.EnvPassthrough, plan.EnvRedact, plan.Env, opts.ParentEnv)
	env = mergeEnv(env, prepared.Env)

	return &Prepared{
		Command:      prepared.Argv[0],
		Args:         append([]string(nil), prepared.Argv[1:]...),
		Dir:          prepared.Workdir,
		Env:          env,
		BootDir:      prepared.PlantedBootDir,
		WorkspaceDir: workspaceDir,
	}, nil
}

func agentLaunchPlan(plan *launch.Plan, workspaceDir string) agentlaunch.LaunchPlan {
	projectID := plan.ProjectID
	if projectID == "" {
		projectID = "project"
	}
	agentID := plan.LogicalAgentID
	if agentID == "" {
		agentID = "agent"
	}
	return agentlaunch.LaunchPlan{
		Project: agentlaunch.ProjectSpec{ID: projectID, Root: plan.RepoRoot},
		Agent:   agentlaunch.AgentSpec{ID: agentID},
		Provider: agentlaunch.ProviderSpec{
			ID:     plan.ProviderBrand,
			Binary: plan.Command,
			Flags:  append([]string(nil), plan.Args...),
			Env:    copyMap(plan.Env),
		},
		Runtime: mapRuntime(plan.RuntimeKind),
		Workspace: agentlaunch.WorkspaceSpec{
			Mode:         agentlaunch.WorkspacePersistent,
			Workdir:      plan.EffectiveWorkRoot(),
			WorkspaceDir: workspaceDir,
		},
		BootProfile: agentlaunch.BootProfileRef{
			Inline: &agentlaunch.BootProfileInline{
				BootPrompt: plan.BootPrompt,
				BootMode:   mapBootMode(plan.BootMode),
			},
		},
		Injection: agentlaunch.InjectionSpec{
			NativeFiles:    nativeFiles(plan.NativeFiles),
			BootDirOverlay: copyMap(plan.BootDirOverlay),
		},
		Mode: agentlaunch.LaunchInteractive,
	}
}

func mapRuntime(runtime string) agentlaunch.RuntimeKind {
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

func mapBootMode(mode string) string {
	switch mode {
	case agentlaunch.BootModeNone, agentlaunch.BootModeStdin, agentlaunch.BootModePlanted:
		return mode
	default:
		return agentlaunch.BootModePlanted
	}
}

func nativeFiles(in []launch.NativeFile) []agentlaunch.NativeFile {
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

func muxEnvMap(env []string) map[string]string {
	if len(env) == 0 {
		return nil
	}
	out := map[string]string{}
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if ok && k != "" {
			out[k] = v
		}
	}
	return out
}

func mergeEnv(base []string, overlay map[string]string) []string {
	out := append([]string(nil), base...)
	for k, v := range overlay {
		prefix := k + "="
		replaced := false
		for i, kv := range out {
			if strings.HasPrefix(kv, prefix) {
				out[i] = prefix + v
				replaced = true
				break
			}
		}
		if !replaced {
			out = append(out, prefix+v)
		}
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
