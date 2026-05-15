package launch

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hollis-labs/tether/internal/config"
)

type Input struct {
	LaunchID            string
	CatalogRoot         string
	SkipPromptFragments bool
}

func Resolve(cat *config.Catalog, in Input) (*Plan, error) {
	l, ok := cat.Launches[in.LaunchID]
	if !ok {
		return nil, fmt.Errorf("launch %q not found", in.LaunchID)
	}
	proj := cat.Projects[l.Project]
	agent := cat.Agents[l.Agent]
	prov := cat.Providers[l.Provider]

	var fragments []string
	if !in.SkipPromptFragments {
		if l.Prompt.IncludeProjectBoot {
			fragments = append(fragments, proj.BootFragments...)
		}
		if l.Prompt.IncludeAgentBoot {
			fragments = append(fragments, agent.BootFragments...)
		}
		if l.Prompt.IncludeKnowledgeBase {
			fragments = append(fragments, proj.KnowledgeBase...)
		}
	}
	boot, err := Compose(in.CatalogRoot, fragments)
	if err != nil {
		return nil, fmt.Errorf("compose boot prompt: %w", err)
	}
	if prov.Bootstrap.PromptPrefix != "" {
		boot = prov.Bootstrap.PromptPrefix + "\n" + boot
	}

	// Carry only the explicit overrides into the plan. The adapter composes
	// the effective child env at launch time per prov.Env.Mode, so parent
	// values are never materialized into plan.Env (and thus never persisted
	// in launch_plans). See internal/provider/env.go.
	overrides := map[string]string{}
	for k, v := range l.Overrides.Env {
		overrides[k] = v
	}

	// Resolve MCP server filter: project wins over launch. Injected as
	// MUX_MCP_SERVERS so the spawned agent's mux mcp --proxy process picks it
	// up without requiring per-agent ~/.claude.json changes.
	mcpServers := proj.MCP.Servers
	if len(mcpServers) == 0 {
		mcpServers = l.MCP.Servers
	}
	if len(mcpServers) > 0 {
		overrides["MUX_MCP_SERVERS"] = strings.Join(mcpServers, ",")
	}

	mode := prov.Env.Mode
	if mode == "" {
		mode = "merge"
	}

	writeHome := l.Workspace.WriteHome
	if writeHome == "" {
		writeHome = proj.Workspace.SessionRoot
	}
	writeHome = config.Expand(writeHome)
	if writeHome == "" {
		writeHome = filepath.Join(config.Expand(cat.Global.Catalog.Defaults.WorkspaceRoot), proj.ID)
	}

	workspaceMode := l.Workspace.Mode
	if workspaceMode == "" {
		workspaceMode = proj.Workspace.DefaultMode
	}
	if workspaceMode == "" {
		workspaceMode = "worktree"
	}

	nativeFiles, bootDirOverlay, err := resolveInjection(in.CatalogRoot, l.Injection)
	if err != nil {
		return nil, fmt.Errorf("resolve injection: %w", err)
	}

	return &Plan{
		LaunchID:       l.ID,
		ProjectID:      proj.ID,
		LogicalAgentID: agent.ID,
		ProviderID:     prov.ID,
		ProviderBrand:  prov.ProviderBrand(),
		RuntimeKind:    prov.EffectiveRuntimeKind(),
		RepoRoot:       config.Expand(proj.RepoRoot),
		WriteHome:      writeHome,
		WorkspaceMode:  workspaceMode,
		WorktreeBase:   config.Expand(proj.Workspace.WorktreeBase),
		WorktreeName:   l.Workspace.WorktreeName,
		Command:        prov.Command,
		Args:           prov.Args,
		Env:            overrides,
		EnvMode:        mode,
		EnvPassthrough: prov.Env.Passthrough,
		EnvRedact:      prov.Env.Redact,
		BootPrompt:     boot,
		BootMode:       prov.Bootstrap.Mode,
		NativeFiles:    nativeFiles,
		BootDirOverlay: bootDirOverlay,
	}, nil
}

func resolveInjection(catalogRoot string, in config.LaunchInjection) ([]NativeFile, map[string]string, error) {
	nativeFiles := make([]NativeFile, 0, len(in.NativeFiles))
	for _, f := range in.NativeFiles {
		content, err := resolveInjectedContent(catalogRoot, f)
		if err != nil {
			return nil, nil, err
		}
		kind := f.Kind
		if kind == "" {
			kind = "raw"
		}
		nativeFiles = append(nativeFiles, NativeFile{
			Kind:    kind,
			ID:      f.ID,
			RelPath: filepath.ToSlash(f.RelPath),
			Content: content,
			Mode:    f.Mode,
		})
	}

	overlay := map[string]string{}
	for _, f := range in.BootDirOverlay {
		rel := filepath.ToSlash(f.RelPath)
		if rel == "" {
			return nil, nil, fmt.Errorf("boot_dir_overlay entry missing rel_path")
		}
		content, err := resolveInjectedContent(catalogRoot, f)
		if err != nil {
			return nil, nil, err
		}
		overlay[rel] = content
	}
	if len(overlay) == 0 {
		overlay = nil
	}
	return nativeFiles, overlay, nil
}

func resolveInjectedContent(catalogRoot string, f config.InjectedFile) (string, error) {
	if f.Content != "" && f.Source != "" {
		return "", fmt.Errorf("injected file %q sets both content and source", f.RelPath)
	}
	if f.Source == "" {
		return f.Content, nil
	}
	path := os.ExpandEnv(f.Source)
	if strings.HasPrefix(path, "~") {
		path = config.Expand(path)
	} else if !filepath.IsAbs(path) {
		path = filepath.Join(catalogRoot, path)
	} else {
		path = config.Expand(path)
	}
	b, err := os.ReadFile(path) //nolint:gosec // G304: operator-provided catalog injection source.
	if err != nil {
		return "", fmt.Errorf("read injected source %s: %w", path, err)
	}
	return string(b), nil
}
