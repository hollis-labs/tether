package launchprofile

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// CompositionInput carries the request parameters for resolving a launch composition.
//
// In this model, Scope (project/workspace context), Provider, and WorkspaceMode
// are launch INPUTS, not baked into profile identity. This eliminates the
// Project × Provider × WorkspaceMode combinatorial explosion.
type CompositionInput struct {
	// Target is the profile ID to resolve through the cascade. Required.
	Target string `json:"target"`

	// Scope is the project / repository context ID or path.
	// When set, resolves the LaunchContext for the launch.
	Scope string `json:"scope,omitempty"`

	// Context is an optional pre-resolved or inline LaunchContext.
	Context *LaunchContext `json:"context,omitempty"`

	// Provider overrides the profile's declared provider (e.g. "claude-code", "codex").
	Provider string `json:"provider,omitempty"`

	// WorkspaceMode overrides the context/profile default workspace mode (e.g. "worktree", "direct").
	WorkspaceMode string `json:"workspace_mode,omitempty"`

	// WorktreeName is the specific worktree branch/folder name when WorkspaceMode is "worktree".
	WorktreeName string `json:"worktree_name,omitempty"`

	// Additional skills to merge additively.
	Skills []string `json:"skills,omitempty"`

	// Additional prompts to merge additively.
	Prompts []string `json:"prompts,omitempty"`

	// Explicit environment overrides (KEY: VALUE).
	Env map[string]string `json:"env,omitempty"`

	// SystemPromptOverride replaces the composed system prompt.
	SystemPromptOverride string `json:"system_prompt_override,omitempty"`

	// PromptAppend appends instructions to the composed prompt.
	PromptAppend string `json:"prompt_append,omitempty"`

	// NativeFiles and BootDirOverlay for file injections.
	NativeFiles    []NativeFile      `json:"native_files,omitempty"`
	BootDirOverlay map[string]string `json:"boot_dir_overlay,omitempty"`
}

// ResolvedComposition is the fully resolved result of a launch composition.
type ResolvedComposition struct {
	Profile        *LaunchProfile    `json:"profile"`
	Context        *LaunchContext    `json:"context,omitempty"`
	Provider       string            `json:"provider"`
	WorkspaceMode  string            `json:"workspace_mode"`
	WorktreeName   string            `json:"worktree_name,omitempty"`
	SystemPrompt   string            `json:"system_prompt"`
	AgentPrompt    string            `json:"agent_prompt,omitempty"`
	BootPrompt     string            `json:"boot_prompt"`
	Skills         []string          `json:"skills,omitempty"`
	Prompts        []string          `json:"prompts,omitempty"`
	Roles          []string          `json:"roles,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	NativeFiles    []NativeFile      `json:"native_files,omitempty"`
	BootDirOverlay map[string]string `json:"boot_dir_overlay,omitempty"`
	LineageChain   []string          `json:"lineage_chain"`
}

// Snapshot is an immutable, deterministic snapshot of a resolved launch.
// It can be persisted alongside session records to guarantee reproducible audits and resumes.
type Snapshot struct {
	Digest      string              `json:"digest"` // sha256:<hex>
	ResolvedAt  time.Time           `json:"resolved_at"`
	Composition ResolvedComposition `json:"composition"`
}

// ComputeDigest computes a deterministic SHA-256 digest over the canonical JSON of the composition.
func (c *ResolvedComposition) ComputeDigest() (string, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("marshal composition for digest: %w", err)
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// ToSnapshot converts a ResolvedComposition into an immutable Snapshot with a verified digest.
func (c *ResolvedComposition) ToSnapshot() (*Snapshot, error) {
	digest, err := c.ComputeDigest()
	if err != nil {
		return nil, err
	}
	return &Snapshot{
		Digest:      digest,
		ResolvedAt:  time.Now().UTC(),
		Composition: *c,
	}, nil
}

// ToPlan converts the resolved composition into an execution Plan consumed by session runtimes.
func (c *ResolvedComposition) ToPlan(launchID string) *Plan {
	if launchID == "" {
		launchID = c.Profile.ID
	}
	var projectID, repoRoot, writeHome, worktreeBase string
	if c.Context != nil {
		projectID = c.Context.ID
		repoRoot = c.Context.RepoRoot
		writeHome = c.Context.Workspace.SessionRoot
		worktreeBase = c.Context.Workspace.WorktreeBase
	}

	planEnv := make(map[string]string, len(c.Env))
	for k, v := range c.Env {
		planEnv[k] = v
	}

	digest, _ := c.ComputeDigest()

	return &Plan{
		LaunchID:         launchID,
		ProjectID:        projectID,
		LogicalAgentID:   c.Profile.ID,
		ProviderID:       c.Provider,
		PermissionMode:   c.Profile.Permissions.PermissionMode,
		RepoRoot:         repoRoot,
		WriteHome:        writeHome,
		WorkspaceMode:    c.WorkspaceMode,
		WorktreeBase:     worktreeBase,
		WorktreeName:     c.WorktreeName,
		Env:              planEnv,
		EnvMode:          "merge",
		BootPrompt:       c.BootPrompt,
		BootPromptAppend: "",
		BootMode:         "eval",
		NativeFiles:      c.NativeFiles,
		BootDirOverlay:   c.BootDirOverlay,
		Shared: &SharedLaunchState{
			PlanHash:      digest,
			ProviderID:    c.Provider,
			WorkspaceMode: c.WorkspaceMode,
		},
	}
}

// Resolver executes compositional launch resolution against a Source.
type Resolver struct {
	source Source
}

// NewResolver creates a Resolver using the given Source.
func NewResolver(source Source) *Resolver {
	return &Resolver{source: source}
}

// Resolve processes a CompositionInput: walks extends chains, merges keyed collections,
// applies scalar replacements, folds context and overrides, and generates the final composition.
func (r *Resolver) Resolve(ctx context.Context, in CompositionInput) (*ResolvedComposition, error) {
	if in.Target == "" {
		return nil, fmt.Errorf("target launch profile ID is required")
	}

	// 1. Walk inheritance chain (leaf to root)
	chain, lineage, err := r.walkExtendsChain(ctx, in.Target)
	if err != nil {
		return nil, err
	}

	// 2. Fold root-to-leaf (ancestor-first)
	resolved := r.foldChain(chain)

	// 3. Resolve Scope / LaunchContext
	var launchCtx *LaunchContext
	if in.Context != nil {
		launchCtx = in.Context
	} else if in.Scope != "" {
		c, err := r.source.GetContext(ctx, in.Scope)
		if err == nil {
			launchCtx = c
		} else if !errors.Is(err, ErrContextNotFound) {
			return nil, fmt.Errorf("resolving context %q: %w", in.Scope, err)
		}
	}

	// 4. Apply launch-time inputs and overrides
	effectiveProvider := resolved.Provider
	if in.Provider != "" {
		effectiveProvider = in.Provider
	}

	effectiveWorkspaceMode := in.WorkspaceMode
	if effectiveWorkspaceMode == "" && launchCtx != nil && launchCtx.Workspace.DefaultMode != "" {
		effectiveWorkspaceMode = launchCtx.Workspace.DefaultMode
	}
	if effectiveWorkspaceMode == "" {
		effectiveWorkspaceMode = "worktree"
	}

	// Merge additional skills and prompts
	skills := appendUnique(resolved.Skills, in.Skills...)
	prompts := appendUnique(resolved.Prompts, in.Prompts...)

	// Merge environment
	env := make(map[string]string, len(resolved.Env)+len(in.Env))
	for k, v := range resolved.Env {
		env[k] = v
	}
	for k, v := range in.Env {
		env[k] = v
	}

	// Inject MCP servers if defined on context
	if launchCtx != nil && len(launchCtx.MCP.Servers) > 0 {
		if _, exists := env["MUX_MCP_SERVERS"]; !exists {
			env["MUX_MCP_SERVERS"] = strings.Join(launchCtx.MCP.Servers, ",")
		}
	}

	systemPrompt := resolved.SystemPrompt
	if in.SystemPromptOverride != "" {
		systemPrompt = in.SystemPromptOverride
	}

	// Compose full boot prompt
	var promptFragments []string
	if systemPrompt != "" {
		promptFragments = append(promptFragments, systemPrompt)
	}
	if resolved.AgentPrompt != "" {
		promptFragments = append(promptFragments, resolved.AgentPrompt)
	}
	promptFragments = append(promptFragments, resolved.BootFragments...)
	if launchCtx != nil {
		promptFragments = append(promptFragments, launchCtx.KnowledgeBase...)
		promptFragments = append(promptFragments, launchCtx.BootFragments...)
	}
	if resolved.Body != "" {
		promptFragments = append(promptFragments, resolved.Body)
	}
	if in.PromptAppend != "" {
		promptFragments = append(promptFragments, in.PromptAppend)
	}
	bootPrompt := strings.Join(promptFragments, "\n\n")

	// Merge native files and boot-dir overlay
	nativeFiles := append([]NativeFile(nil), in.NativeFiles...)
	var bootOverlay map[string]string
	if len(in.BootDirOverlay) > 0 {
		bootOverlay = make(map[string]string, len(in.BootDirOverlay))
		for k, v := range in.BootDirOverlay {
			bootOverlay[k] = v
		}
	}

	return &ResolvedComposition{
		Profile:        resolved,
		Context:        launchCtx,
		Provider:       effectiveProvider,
		WorkspaceMode:  effectiveWorkspaceMode,
		WorktreeName:   in.WorktreeName,
		SystemPrompt:   systemPrompt,
		AgentPrompt:    resolved.AgentPrompt,
		BootPrompt:     bootPrompt,
		Skills:         skills,
		Prompts:        prompts,
		Roles:          resolved.Roles,
		Env:            env,
		NativeFiles:    nativeFiles,
		BootDirOverlay: bootOverlay,
		LineageChain:   lineage,
	}, nil
}

// ResolveSnapshot resolves the composition and wraps it in an immutable Snapshot.
func (r *Resolver) ResolveSnapshot(ctx context.Context, in CompositionInput) (*Snapshot, error) {
	comp, err := r.Resolve(ctx, in)
	if err != nil {
		return nil, err
	}
	return comp.ToSnapshot()
}

func (r *Resolver) walkExtendsChain(ctx context.Context, target string) ([]*LaunchProfile, []string, error) {
	var chain []*LaunchProfile
	var lineage []string
	visited := make(map[string]bool)

	currID := target
	for currID != "" {
		if visited[currID] {
			return nil, nil, fmt.Errorf("%w: cycle at %q in chain %s", ErrCycle, currID, strings.Join(lineage, " -> "))
		}
		visited[currID] = true
		lineage = append(lineage, currID)

		profile, err := r.source.GetProfile(ctx, currID)
		if err != nil {
			return nil, nil, fmt.Errorf("resolving profile %q in chain: %w", currID, err)
		}
		chain = append(chain, profile)
		currID = profile.Extends
	}

	// Reverse so root is first, leaf is last
	slices.Reverse(chain)
	slices.Reverse(lineage)
	return chain, lineage, nil
}

func (r *Resolver) foldChain(chain []*LaunchProfile) *LaunchProfile {
	resolved := &LaunchProfile{
		ProviderOverrides: make(map[string]ProviderOverride),
		Env:               make(map[string]string),
	}

	for _, p := range chain {
		// ID becomes the leaf ID
		resolved.ID = p.ID

		// Scalars: closest-wins (non-empty replaces)
		if p.Name != "" {
			resolved.Name = p.Name
		}
		if p.Description != "" {
			resolved.Description = p.Description
		}
		if p.Provider != "" {
			resolved.Provider = p.Provider
		}
		if p.Model != "" {
			resolved.Model = p.Model
		}
		if p.SystemPrompt != "" {
			resolved.SystemPrompt = p.SystemPrompt
		}
		if p.AgentPrompt != "" {
			resolved.AgentPrompt = p.AgentPrompt
		}
		if p.Permissions.PermissionMode != "" {
			resolved.Permissions.PermissionMode = p.Permissions.PermissionMode
		}
		if p.Permissions.Network {
			resolved.Permissions.Network = true
		}
		if p.Permissions.DefaultSandbox != "" {
			resolved.Permissions.DefaultSandbox = p.Permissions.DefaultSandbox
		}

		// Keyed collections: merge additively
		resolved.Skills = appendUnique(resolved.Skills, p.Skills...)
		resolved.Prompts = appendUnique(resolved.Prompts, p.Prompts...)
		resolved.Roles = appendUnique(resolved.Roles, p.Roles...)
		resolved.ContextFiles = appendUnique(resolved.ContextFiles, p.ContextFiles...)
		resolved.BootFragments = append(resolved.BootFragments, p.BootFragments...)

		// Environment variables: merge by key (descendant wins)
		for k, v := range p.Env {
			resolved.Env[k] = v
		}

		// Provider overrides: merge by provider ID
		for provID, override := range p.ProviderOverrides {
			existing, exists := resolved.ProviderOverrides[provID]
			if !exists {
				existing = ProviderOverride{
					Env: make(map[string]string),
				}
			}
			existing.ExtraArgs = appendUnique(existing.ExtraArgs, override.ExtraArgs...)
			if existing.Env == nil {
				existing.Env = make(map[string]string)
			}
			for k, v := range override.Env {
				existing.Env[k] = v
			}
			resolved.ProviderOverrides[provID] = existing
		}

		// Body: concatenate root to leaf
		if p.Body != "" {
			if resolved.Body == "" {
				resolved.Body = p.Body
			} else {
				resolved.Body = resolved.Body + "\n\n" + p.Body
			}
		}

		// Spec: merge map
		if len(p.Spec) > 0 {
			if resolved.Spec == nil {
				resolved.Spec = make(map[string]any)
			}
			for k, v := range p.Spec {
				resolved.Spec[k] = v
			}
		}
	}

	return resolved
}

func appendUnique(target []string, additions ...string) []string {
	out := append([]string(nil), target...)
	for _, s := range additions {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}
