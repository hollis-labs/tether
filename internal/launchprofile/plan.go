package launchprofile

// Plan is the resolved launch contract the provider adapter consumes. Env
// fields describe a *policy* for composing the child's environment, not a
// pre-flattened result — the adapter applies the policy against the live
// os.Environ() at start time so fresh parent-env state is captured per
// launch and overrides are the only values persisted in the plan.
type Plan struct {
	Lifecycle      *LifecyclePolicy `json:"lifecycle,omitempty"`
	Route          *Route           `json:"route,omitempty"`
	LaunchID       string           `json:"launch_id"`
	ProjectID      string           `json:"project_id"`
	LogicalAgentID string           `json:"logical_agent_id"`
	// TeamMember is set only by the internal team create path. Catalog identity
	// still resolves sandbox policy; the enrollment adapter owns actor binding.
	TeamMember    bool   `json:"team_member,omitempty"`
	ProviderID    string `json:"provider_id"`
	ProviderBrand string `json:"provider_brand,omitempty"`
	RuntimeKind   string `json:"runtime_kind,omitempty"`
	// ResumeProviderSessionID is set only by explicit or boot recovery. LaunchSession
	// feeds it into agentsessions.StartOptions.SessionIDPreset so normal
	// launches never become implicit provider-native resumes.
	ResumeProviderSessionID string `json:"resume_provider_session_id,omitempty"`
	// NativeResumeOnly is an explicit fail-closed recovery decision. It never
	// permits cold fallback or automatic submission of a recovery turn.
	NativeResumeOnly bool `json:"native_resume_only,omitempty"`
	// ResumeSourceSessionID identifies the canonical source of native state and
	// interrupted-work evidence. It is not a provider ID or an authority grant.
	// NativeStateRoot records the actual prepared boot directory. It carries
	// no credentials or permission grant and is never inferred from a scan.
	NativeStateRoot       string `json:"native_state_root,omitempty"`
	ResumeSourceSessionID string `json:"resume_source_session_id,omitempty"`
	// RecoveryCursors describe channel context actually included in the control
	// turn. They advance only after accepted submission; no inbox is acknowledged.
	RecoveryCursors map[string]int64 `json:"recovery_cursors,omitempty"`
	RecoveryPrompt  string           `json:"recovery_prompt,omitempty"`
	// RecoveryActorURI is context addressing for a retained enrolled member,
	// never an enrollment or permission grant.
	RecoveryActorURI string `json:"recovery_actor_uri,omitempty"`
	// PermissionMode is the resolved Claude Code permission posture for the
	// launched agent ("bypass" or "default"), computed by the launch
	// resolver from the agent's permissions.permission_mode falling back to
	// global defaults. For claude providers the resolver also threads the
	// concrete CLI flags into Args; this field records the decision for
	// inspection and for non-claude providers that may map it differently.
	PermissionMode string `json:"permission_mode,omitempty"`
	// SandboxProfile is the sandbox profile an agent_file or agent_inline
	// override set at create, when it differs from the catalog agent's
	// default_sandbox. LaunchSession applies it instead of the catalog
	// agent's, so an override is never silently dropped (CW-20261001-0145).
	// Empty means no override: the catalog agent's profile applies, as for
	// every plan created before this field existed.
	SandboxProfile string `json:"sandbox_profile,omitempty"`
	// ExtractRefs records whether proxy-side identifier extraction (--extract-refs)
	// was enabled for this launch plan.
	ExtractRefs     bool     `json:"extract_refs,omitempty"`
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
	// CallerEnv names (never the values) the Env keys that came from somewhere
	// an operator's catalog does not control: a caller's override JSON, or any
	// provider_overrides env in the effective agent, which may come from an
	// agent_file, an inline agent, or an agent definition in a user or project
	// layer that an agent can write (CW-20261001-0142). A launch with any is
	// not left to codex's own sandbox: variables such as PATH, TMPDIR and
	// LD_PRELOAD defeat it. Sorted, no duplicates.
	CallerEnv []string `json:"caller_env,omitempty"`
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
	// BootPromptAppend preserves caller-supplied prompt addenda across late
	// boot-profile regeneration after workspace materialization.
	BootPromptAppend string `json:"boot_prompt_append,omitempty"`
	BootMode         string `json:"boot_mode"`

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
