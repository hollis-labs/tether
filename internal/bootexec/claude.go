package bootexec

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/google/uuid"
	gop "github.com/hollis-labs/substrate/harness/adapters/provider"
	"github.com/hollis-labs/substrate/harness/agentlaunch"
	"github.com/hollis-labs/substrate/harness/agentlaunch/launcher"
	providerplant "github.com/hollis-labs/substrate/harness/agentlaunch/planting"

	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/launchartifacts"
	tetherprovider "github.com/hollis-labs/tether/internal/provider"
)

// Options carries direct-exec-only inputs that are normally supplied by the
// agentsessions StartOptions path.
type Options struct {
	BootDirRoot      string
	APIKeyHelperPath string
	TetherCommand    string
	TetherArgs       []string
	TetherEnv        []string
	ParentEnv        []string

	// SpecPlan, when non-nil, is the agentlaunch.LaunchPlan that feeds
	// launcher.Compile. The S5 "spec" launch engine sets it to the plan
	// resolved by internal/specresolve; the legacy "catalog" engine leaves
	// it nil, in which case PrepareClaudeTUI builds the plan from the
	// launch.Plan via agentLaunchPlan exactly as before. The toggle changes
	// only this plan's provenance — Compile/Prepare/Plant are unchanged.
	SpecPlan *agentlaunch.LaunchPlan
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
func PrepareClaudeTUI(plan *launch.Plan, opts Options) (result *Prepared, err error) {
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

	var lp agentlaunch.LaunchPlan
	if opts.SpecPlan != nil {
		// S5 "spec" engine: use the caller's Spec-resolved plan. The daemon
		// owns the materialized workspace dir; the resolver does not know
		// it, so fold it on here just as the catalog path does.
		lp = *opts.SpecPlan
		lp.Workspace.WorkspaceDir = workspaceDir
	} else {
		lp = agentLaunchPlan(plan, workspaceDir)
	}
	lp.Workspace.TempPrefix = root
	compiled, err := launcher.Compile(context.Background(), lp)
	if err != nil {
		_ = os.RemoveAll(workspaceDir)
		return nil, err
	}
	// This accepted direct-exec operation owns its resolved plan and the fresh
	// allocator call. It has no daemon session row or fabricated principal.
	version, err := launchartifacts.Digest(plan)
	if err != nil {
		return nil, err
	}
	admission := launchartifacts.Admission{OperationID: uuid.NewString(), DecisionID: "tether.boot-exec:" + version,
		Version: version, Owner: fmt.Sprintf("tether-boot-exec-uid:%d", os.Geteuid()), ControlParent: root, LocalFilesystem: true,
		Validate: func(ctx context.Context) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			current, err := launchartifacts.Digest(plan)
			if err != nil || current != version {
				return fmt.Errorf("boot-exec artifact admission: accepted plan changed")
			}
			return nil
		}}
	prepared, custody, err := launchartifacts.Prepare(context.Background(), compiled, admission)
	if err != nil {
		_ = os.RemoveAll(workspaceDir)
		return nil, err
	}
	defer func() {
		err = errors.Join(err, custody.Close())
		if err != nil {
			result = nil
		}
	}()
	prepared.PlantContext.SelfMCPCommand = opts.TetherCommand
	prepared.PlantContext.SelfMCPArgs = append([]string(nil), opts.TetherArgs...)
	prepared.PlantContext.SelfMCPEnv = tetherEnvMap(opts.TetherEnv)

	adapter := gop.NewClaudeAdapterPTY()
	adapter.ApiKeyHelperPath = opts.APIKeyHelperPath
	// The plan's permission posture (Provider.Permission) needs no threading
	// onto this explicit adapter: providerplant maps it into the launch's
	// argv whichever adapter plants (CW-20261001-0156).
	if err := providerplant.Plant(context.Background(), prepared, providerplant.WithAdapter(adapter), providerplant.WithArtifactAuthorization(custody.Authorize)); err != nil {
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
	return launch.AgentLaunchPlan(plan, workspaceDir)
}

func tetherEnvMap(env []string) map[string]string {
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
