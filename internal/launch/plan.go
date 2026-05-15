package launch

// Plan is the resolved launch contract the provider adapter consumes. Env
// fields describe a *policy* for composing the child's environment, not a
// pre-flattened result — the adapter applies the policy against the live
// os.Environ() at start time so fresh parent-env state is captured per
// launch and overrides are the only values persisted in the plan.
type Plan struct {
	LaunchID        string   `json:"launch_id"`
	ProjectID       string   `json:"project_id"`
	LogicalAgentID  string   `json:"logical_agent_id"`
	ProviderID      string   `json:"provider_id"`
	ProviderBrand   string   `json:"provider_brand,omitempty"`
	RuntimeKind     string   `json:"runtime_kind,omitempty"`
	RepoRoot        string   `json:"repo_root"`
	WorkRoot        string   `json:"work_root,omitempty"`
	WriteHome       string   `json:"write_home"`
	WorkspaceMode   string   `json:"workspace_mode,omitempty"`
	WorktreeBase    string   `json:"worktree_base,omitempty"`
	WorktreeName    string   `json:"worktree_name,omitempty"`
	BootProfileFile string   `json:"boot_profile_file,omitempty"`
	Command         string   `json:"command"`
	Args            []string `json:"args"`

	// Env holds explicit overrides (from the launch's overrides.env block).
	// The adapter applies these last, after composing the base environment
	// per EnvMode. Parent-inherited values are NOT materialized here.
	Env map[string]string `json:"env"`
	// EnvMode selects the env composition strategy; "merge" (default) or
	// "whitelist". See internal/provider/env.go for semantics.
	EnvMode string `json:"env_mode"`
	// EnvPassthrough lists keys pulled from the parent environment in
	// whitelist mode. Ignored in merge mode.
	EnvPassthrough []string `json:"env_passthrough,omitempty"`
	// EnvRedact lists keys to scrub from the parent environment in merge
	// mode. Ignored in whitelist mode.
	EnvRedact []string `json:"env_redact,omitempty"`

	BootPrompt string `json:"boot_prompt"`
	BootMode   string `json:"boot_mode"`

	// NativeFiles and BootDirOverlay carry injected file content (from catalog
	// injection and caller-provided injection alike).
	//
	// SECURITY — PERSISTED AT REST, NON-SECRET ONLY. The whole Plan, including
	// these fields, is persisted verbatim as JSON in the launch_plans table.
	// Injected content is therefore at-rest data. Never route secrets through
	// injection — see config.LaunchInjection. Secrets belong in provider env
	// passthrough/whitelist mode (EnvPassthrough), which pulls from the live
	// parent environment and is NOT materialized into the plan.
	NativeFiles    []NativeFile       `json:"native_files,omitempty"`
	BootDirOverlay map[string]string  `json:"boot_dir_overlay,omitempty"`
	Shared         *SharedLaunchState `json:"shared_launch,omitempty"`
}

type NativeFile struct {
	Kind    string `json:"kind"`
	ID      string `json:"id,omitempty"`
	RelPath string `json:"rel_path,omitempty"`
	Content string `json:"content,omitempty"`
	Mode    uint32 `json:"mode,omitempty"`
}

type SharedLaunchState struct {
	PlanHash        string `json:"plan_hash,omitempty"`
	CompilerVersion string `json:"compiler_version,omitempty"`
	ProviderID      string `json:"provider_id,omitempty"`
	RuntimeKind     string `json:"runtime_kind,omitempty"`
	WorkspaceMode   string `json:"workspace_mode,omitempty"`
	BootFile        string `json:"boot_file,omitempty"`
	TransientFile   string `json:"transient_file,omitempty"`
	MCPFile         string `json:"mcp_file,omitempty"`
}

func (p *Plan) EffectiveWorkRoot() string {
	if p != nil && p.WorkRoot != "" {
		return p.WorkRoot
	}
	if p != nil {
		return p.RepoRoot
	}
	return ""
}
