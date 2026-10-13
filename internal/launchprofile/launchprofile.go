package launchprofile

// MCPConfig holds per-project or per-launch MCP proxy settings that are
// injected into agent sessions at launch time. Servers lists the upstream MCP
// server IDs to expose as native tools (sets TETHER_MCP_SERVERS). Omitted or
// null inherits; an explicit empty list grants no upstreams.
// ExtractRefs enables proxy-side identifier extraction (--extract-refs).
type MCPConfig struct {
	Servers     []string `yaml:"servers" json:"servers"`
	ExtractRefs *bool    `yaml:"extract_refs,omitempty" json:"extract_refs,omitempty"`
}

// LaunchContext captures launch-time environment context: repository root,
// tracking root, workspace defaults, and MCP proxy settings. Distinct from
// registry.Project (identity object).
type LaunchContext struct {
	ID       string `yaml:"id" json:"id"`
	Name     string `yaml:"name" json:"name"`
	RepoRoot string `yaml:"repo_root" json:"repo_root"`
	// RepoURL is the project's git remote (e.g. https://github.com/org/repo.git). RepoRoot is a path on one
	// machine; RepoURL is what a remote worker clones to materialize its own checkout. Optional.
	RepoURL       string        `yaml:"repo_url,omitempty" json:"repo_url,omitempty"`
	TrackingRoot  string        `yaml:"tracking_root" json:"tracking_root"`
	KnowledgeBase []string      `yaml:"knowledge_base" json:"knowledge_base,omitempty"`
	BootFragments []string      `yaml:"boot_fragments" json:"boot_fragments,omitempty"`
	Workspace     WorkspaceSpec `yaml:"workspace" json:"workspace"`
	MCP           MCPConfig     `yaml:"mcp" json:"mcp,omitempty"`
}

// Project is a backward-compatible alias for LaunchContext.
type Project = LaunchContext

type WorkspaceSpec struct {
	DefaultMode  string `yaml:"default_mode" json:"default_mode"`
	WorktreeBase string `yaml:"worktree_base" json:"worktree_base,omitempty"`
	SessionRoot  string `yaml:"session_root" json:"session_root,omitempty"`
}

// LaunchProfile captures the stable launch configuration for an agent:
// role, skills, persona prompts, permissions, and provider overrides.
type LaunchProfile struct {
	Lifecycle     *LifecyclePolicy `yaml:"lifecycle,omitempty" json:"lifecycle,omitempty"`
	Route         *Route           `yaml:"route,omitempty" json:"route,omitempty"`
	ID            string           `yaml:"id" json:"id"`
	Name          string           `yaml:"name" json:"name"`
	Roles         []string         `yaml:"roles" json:"roles,omitempty"`
	Skills        []string         `yaml:"skills" json:"skills,omitempty"`
	ContextFiles  []string         `yaml:"context_files" json:"context_files,omitempty"`
	BootFragments []string         `yaml:"boot_fragments" json:"boot_fragments,omitempty"`
	Permissions   AgentPermissions `yaml:"permissions" json:"permissions"`

	// Extends is the ID of an ancestor profile this one inherits from via cascade.
	// Empty at the root of an inheritance chain.
	Extends string `yaml:"extends,omitempty" json:"extends,omitempty"`

	// Abstract marks a profile intended only for extension, not direct launching.
	Abstract bool `yaml:"abstract,omitempty" json:"abstract,omitempty"`

	// Description is human-readable documentation for this profile.
	Description string `yaml:"description,omitempty" json:"description,omitempty"`

	// Provider identifies the default execution harness (e.g. "claude-code", "codex").
	// Can be overridden at launch time.
	Provider string `yaml:"provider,omitempty" json:"provider,omitempty"`

	// Model specifies the preferred model identifier (e.g. "claude-3-7-sonnet-20250219").
	Model string `yaml:"model,omitempty" json:"model,omitempty"`

	// Prompts lists named prompt templates or commands associated with this profile.
	Prompts []string `yaml:"prompts,omitempty" json:"prompts,omitempty"`

	// Env holds profile-level environment variables to inject.
	Env map[string]string `yaml:"env,omitempty" json:"env,omitempty"`

	// Spec carries open manifest configurations (e.g. settings, tools).
	Spec map[string]any `yaml:"spec,omitempty" json:"spec,omitempty"`

	// Body holds inline markdown / instructions concatenated during cascade resolution.
	Body string `yaml:"body,omitempty" json:"body,omitempty"`

	// SystemPrompt and AgentPrompt are the agent's persona payload (v005-08).
	// Each may be an inline string (anything containing whitespace, `:`, or
	// starting with text other than a path) or a file path; resolution happens
	// at launch time by the BootDirSpec compilation pipeline. Empty = unset.
	SystemPrompt string `yaml:"system_prompt" json:"system_prompt,omitempty"`
	AgentPrompt  string `yaml:"agent_prompt" json:"agent_prompt,omitempty"`

	// ProviderOverrides applies per-provider tweaks at launch time, keyed by
	// provider ID (e.g. "claude-code", "codex-app-server"). Missing keys mean
	// the agent uses provider defaults verbatim.
	ProviderOverrides map[string]ProviderOverride `yaml:"provider_overrides" json:"provider_overrides,omitempty"`

	// Recovery controls daemon-boot recovery of this agent: "never" (the
	// default when empty) or "auto". Only "auto" lets boot recovery resume a
	// logical agent that has no live session.
	Recovery string `yaml:"recovery,omitempty" json:"recovery,omitempty"`

	// URN names the stable registry identity (msg://agent/<authority>/<id>) this
	// agent's worker sessions act as. It is looked up in the registry, never
	// minted from here, and is NOT inherited through extends: identity belongs
	// to one agent. Read only from the catalog agent, never from an
	// agent_file/agent_inline override. Optional.
	URN string `yaml:"urn,omitempty" json:"urn,omitempty"`

	// MCPScopes are the scopes of the session-bound credential the hub mints for
	// this agent's worker sessions (for example message.write, registry.write,
	// groups.write). Empty means the default set. Closest definition wins.
	MCPScopes []string `yaml:"mcp_scopes,omitempty" json:"mcp_scopes,omitempty"`

	// Enabled turns the agent off without removing its file: false blocks new
	// launches and chat sends for it, and never touches a session that is
	// already running. Absent means enabled. It is per agent and not
	// inherited through extends.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
}

// IsEnabled reports whether the agent may be launched; absent means yes.
func (p LaunchProfile) IsEnabled() bool { return p.Enabled == nil || *p.Enabled }

// DefaultMCPScopes are the session credential scopes of a worker session whose
// agent declares no mcp_scopes. They match the static scopes workers used before
// per-session credentials; the read-only forum and channel tools need no scope.
var DefaultMCPScopes = []string{"message.write", "registry.write", "groups.write"}

// ValidMCPScope reports whether scope may appear in an agent's mcp_scopes.
func ValidMCPScope(scope string) bool {
	switch scope {
	case "session.write", "message.write", "registry.write", "groups.write", "catalog.write", "ai.invoke", "team.write":
		return true
	}
	return false
}

// Boot-recovery policy values for LaunchProfile.Recovery.
const (
	RecoveryNever = "never"
	RecoveryAuto  = "auto"
)

// ValidRecovery reports whether v is an accepted recovery value; empty means
// the default (never).
func ValidRecovery(v string) bool {
	return v == "" || v == RecoveryNever || v == RecoveryAuto
}

// Agent is a backward-compatible alias for LaunchProfile.
type Agent = LaunchProfile

type AgentPermissions struct {
	Network        bool   `yaml:"network" json:"network"`
	DefaultSandbox string `yaml:"default_sandbox" json:"default_sandbox,omitempty"`
	// PermissionMode overrides the global defaults.permission_mode for
	// this agent: "bypass" or "default". Empty inherits the global
	// default. See config.EffectivePermissionMode.
	PermissionMode string `yaml:"permission_mode" json:"permission_mode,omitempty"`
}

// ProviderOverride carries per-provider customization for an Agent. Fields are
// additive — start narrow and expand when concrete needs surface.
type ProviderOverride struct {
	ExtraArgs []string          `yaml:"extra_args" json:"extra_args,omitempty"`
	Env       map[string]string `yaml:"env" json:"env,omitempty"`
}

// MarshalYAML preserves the distinction between inheritance and an empty grant.
func (m MCPConfig) MarshalYAML() (any, error) {
	out := map[string]any{}
	if m.Servers != nil {
		out["servers"] = m.Servers
	}
	if m.ExtractRefs != nil {
		out["extract_refs"] = m.ExtractRefs
	}
	return out, nil
}
