package workerruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hollis-labs/libs/harness/core/agentlaunch"
	"github.com/hollis-labs/libs/harness/core/agentlaunch/launcher"
	"github.com/hollis-labs/libs/harness/core/agentlaunch/planting"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/launchartifacts"
	"github.com/hollis-labs/tether/internal/workerwire"
)

// ProviderProcess is the private, already-materialized launch consumed by the
// provider bridge inside the shim. Credentials stay in files/environment.
type ProviderProcess struct {
	Provider     string                    `json:"provider"`
	RuntimeKind  string                    `json:"runtime_kind"`
	Binary       string                    `json:"binary"`
	Workdir      string                    `json:"workdir"`
	ProjectDir   string                    `json:"project_dir"`
	WorkspaceDir string                    `json:"workspace_dir"`
	Launch       *agentlaunch.TurnTemplate `json:"launch"`
	ExtraArgs    []string                  `json:"extra_args,omitempty"`
}

func (e *Executor) prepareProvider(ctx context.Context, spec workerwire.LaunchSpec, binary, cwd, root string) (result *agentlaunch.PreparedLaunch, err error) {
	plan := &launch.Plan{ProviderID: spec.Provider, ProviderBrand: spec.Provider, RuntimeKind: spec.RuntimeKind,
		Command: binary, Args: spec.Args, RepoRoot: cwd, WorkspaceMode: "shared", PermissionMode: spec.PermissionMode,
		BootPrompt: spec.BootPrompt, BootMode: "planted", BootDirOverlay: spec.BootFiles, NativeFiles: spec.NativeFiles}
	lp := launch.AgentLaunchPlan(plan, root)
	lp.Workspace.Workdir = cwd
	lp.Workspace.TempPrefix = filepath.Join(root, "boot")
	if err := os.MkdirAll(lp.Workspace.TempPrefix, 0700); err != nil {
		return nil, err
	}
	compiled, err := launcher.Compile(ctx, lp)
	if err != nil {
		return nil, err
	}
	control := filepath.Join(e.Root, "artifact-control")
	if err := os.MkdirAll(control, 0700); err != nil {
		return nil, err
	}
	control, err = filepath.EvalSymlinks(control)
	if err != nil {
		return nil, err
	}
	prepared, custody, err := launchartifacts.Prepare(ctx, compiled, launchartifacts.Admission{
		OperationID: spec.Session, DecisionID: "worker-launch:" + spec.Session, Version: "1", Owner: "tether-worker",
		ControlParent: control, LocalFilesystem: true,
		Validate: func(ctx context.Context) error { return ctx.Err() },
	})
	if err != nil {
		return nil, err
	}
	defer func() {
		err = errors.Join(err, custody.Close())
		if err != nil {
			result = nil
		}
	}()
	// The session gateway resolves the catalog's MCP policy on the hub. The
	// native provider planter owns the provider-specific config and argv.
	prepared.PlantContext.MCPServers = []agentlaunch.MCPServerSpec{{Name: "tether", Command: e.Binary,
		Args: []string{"mcp-http-relay", "--url", e.HubMCPURL, "--token-file", TokenFilePath(filepath.Join(root, "auth"), "tether")}}}
	if reason := e.hubHTTPUnavailable(spec); reason != "" {
		return nil, fmt.Errorf("provider launch requires the session MCP gateway: %s", reason)
	}
	if spec.Provider == "codex" {
		home := spec.Env["CODEX_HOME"]
		if home == "" {
			home = os.Getenv("CODEX_HOME")
		}
		if home == "" {
			user, err := os.UserHomeDir()
			if err != nil {
				return nil, err
			}
			home = filepath.Join(user, ".codex")
		}
		captured, captureErr := launchartifacts.CaptureCodexHome(home)
		if captureErr != nil {
			return nil, fmt.Errorf("worker native Codex home: %w", captureErr)
		}
		defer func() {
			err = errors.Join(err, captured.Close())
			if err != nil {
				result = nil
			}
		}()
		if err := custody.PlantCodex(ctx, prepared, captured); err != nil {
			return nil, err
		}
		return prepared, nil
	}
	if err := planting.Plant(ctx, prepared, planting.WithArtifactAuthorization(custody.Authorize)); err != nil {
		return nil, err
	}
	return prepared, nil
}

func providerProcessArgs(root string, spec workerwire.LaunchSpec, p *agentlaunch.PreparedLaunch) ([]string, error) {
	if p.Launch == nil || len(p.Argv) == 0 {
		return nil, fmt.Errorf("provider launch template unavailable")
	}
	cfg := ProviderProcess{Provider: spec.Provider, RuntimeKind: spec.RuntimeKind, Binary: p.Argv[0], Workdir: p.Workdir,
		WorkspaceDir: root, ProjectDir: p.Compiled.Plan.Project.Root, Launch: p.Launch}
	cfg.Launch = cfg.Launch.Clone()
	cfg.Launch.Convention.Env = nil
	path := filepath.Join(root, "provider.json")
	if err := SaveJSON(path, cfg); err != nil {
		return nil, err
	}
	return []string{"provider-process", "--config", path}, nil
}

func readProviderProcess(path string) (ProviderProcess, error) {
	var cfg ProviderProcess
	b, err := os.ReadFile(path) //nolint:gosec // G304: path names the worker-owned prepared launch, supplied to its internal process.
	if err != nil {
		return cfg, err
	}
	err = json.Unmarshal(b, &cfg)
	if err == nil && (cfg.Launch == nil || !filepath.IsAbs(cfg.Binary) || !filepath.IsAbs(cfg.Workdir) || strings.TrimSpace(cfg.Provider) == "") {
		err = fmt.Errorf("invalid provider process config")
	}
	return cfg, err
}
