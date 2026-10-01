package launch

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/hollis-labs/tether/internal/config"
)

type Input struct {
	LaunchID            string
	CatalogRoot         string
	SkipPromptFragments bool
}

// ErrLaunchNotFound is wrapped by every error Resolve returns for a launch
// ID the catalog does not define, so callers can map it to a not-found
// response with errors.Is. Its own text reads as the tail of that error
// ("launch \"x\" not found").
var ErrLaunchNotFound = errors.New("not found")

// maxKnownLaunchesInError caps how many known launch IDs a not-found error
// lists.
const maxKnownLaunchesInError = 20

func Resolve(cat *config.Catalog, in Input) (*Plan, error) {
	l, ok := cat.Launches[in.LaunchID]
	if !ok {
		return nil, launchNotFound(cat, in.LaunchID)
	}
	proj := cat.Projects[l.Project]
	agent := cat.Agents[l.Agent]
	prov := cat.Providers[l.Provider]

	for owner, ids := range map[string][]string{
		fmt.Sprintf("project %q mcp.servers", l.Project): proj.MCP.Servers,
		fmt.Sprintf("launch %q mcp.servers", l.ID):       l.MCP.Servers,
	} {
		if err := cat.ValidateMCPGrant(owner, ids); err != nil {
			return nil, err
		}
	}

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
	// TETHER_MCP_SERVERS so the spawned agent's tether mcp --proxy process picks it
	// up without requiring per-agent ~/.claude.json changes.
	mcpServers := proj.MCP.Servers
	if mcpServers == nil {
		mcpServers = l.MCP.Servers
	}
	if mcpServers != nil {
		overrides["TETHER_MCP_SERVERS"] = strings.Join(mcpServers, ",")
	}

	if value, set := overrides[MCPServersEnv]; set {
		if err := cat.ValidateMCPGrantEnv(fmt.Sprintf("launch %q effective grant", l.ID), value); err != nil {
			return nil, err
		}
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
		// Explicit catalog workspace_root wins; cat.Paths supplies the
		// go-apppaths fallback only when global.yaml omits the key.
		writeHome = filepath.Join(config.ResolveWorkspaceRoot(cat.Global.Catalog.Defaults, cat.Paths), proj.ID)
	}

	workspaceMode := l.Workspace.Mode
	if workspaceMode == "" {
		workspaceMode = proj.Workspace.DefaultMode
	}
	if workspaceMode == "" {
		workspaceMode = "worktree"
	}

	nativeFiles, bootDirOverlay, err := ResolveInjection(in.CatalogRoot, l.Injection)
	if err != nil {
		return nil, fmt.Errorf("resolve injection: %w", err)
	}

	// Resolve the permission mode (agent override > global default >
	// "default"). It reaches the provider as a posture, not as a flag here:
	// AgentLaunchPlan maps it through config.ProviderPosture onto the shared
	// launch, and agentkit puts the posture's own flags and environment into
	// every turn's argv (CW-20261001-0156).
	//
	// The planted .mcp.json is not spliced in here either: go-providers'
	// projection passes it as --mcp-config <bootDir>/.mcp.json in every
	// Claude mode, and agentkit v0.13.0 resolves every turn's argv from that
	// convention (CW-20260930-0135), so a second --mcp-config from here would
	// repeat it.
	permMode := config.EffectivePermissionMode(cat.Global, agent)
	args := append([]string(nil), prov.Args...)

	extractRefs := config.EffectiveExtractRefs(cat.Global, proj, l)

	return &Plan{
		LaunchID:       l.ID,
		ProjectID:      proj.ID,
		LogicalAgentID: agent.ID,
		ProviderID:     prov.ID,
		ProviderBrand:  prov.ProviderBrand(),
		RuntimeKind:    prov.EffectiveRuntimeKind(),
		PermissionMode: permMode,
		ExtractRefs:    extractRefs,
		RepoRoot:       config.Expand(proj.RepoRoot),
		WriteHome:      writeHome,
		WorkspaceMode:  workspaceMode,
		WorktreeBase:   config.Expand(proj.Workspace.WorktreeBase),
		WorktreeName:   l.Workspace.WorktreeName,
		Command:        prov.Command,
		Args:           args,
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

// ResolveInjection resolves a config.LaunchInjection into the plan-shaped
// native files and boot-dir overlay map. Relative Source paths on injected
// files resolve against root (the catalog/config root) — NOT the process CWD.
//
// It is the single shared resolution entry point: both the catalog launch
// resolver and the caller-provided injection path in package app call it, so
// path-resolution and content/source semantics never drift between the two.
//
// SECURITY: the returned content is persisted at rest in launch.Plan — see
// config.LaunchInjection. Callers must treat injection content as non-secret.
func ResolveInjection(root string, in config.LaunchInjection) ([]NativeFile, map[string]string, error) {
	return resolveInjection(root, in)
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
		if _, exists := overlay[rel]; exists {
			return nil, nil, fmt.Errorf("boot_dir_overlay duplicate rel_path %q", rel)
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

// launchNotFound builds the ErrLaunchNotFound error for id, naming the
// launches the catalog does define so a typo or a stale catalog is obvious
// from the message alone.
func launchNotFound(cat *config.Catalog, id string) error {
	known := make([]string, 0, len(cat.Launches))
	for k := range cat.Launches {
		known = append(known, k)
	}
	slices.Sort(known)
	switch {
	case len(known) == 0:
		return fmt.Errorf("launch %q %w (the catalog defines no launches)", id, ErrLaunchNotFound)
	case len(known) > maxKnownLaunchesInError:
		return fmt.Errorf("launch %q %w (known launches: %s, and %d more)", id, ErrLaunchNotFound,
			strings.Join(known[:maxKnownLaunchesInError], ", "), len(known)-maxKnownLaunchesInError)
	default:
		return fmt.Errorf("launch %q %w (known launches: %s)", id, ErrLaunchNotFound, strings.Join(known, ", "))
	}
}
