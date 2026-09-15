package launchprofile

// MCPConfig holds per-project or per-launch MCP proxy settings that are
// injected into agent sessions at launch time. Servers lists the upstream MCP
// server IDs to expose as native tools (sets MUX_MCP_SERVERS). Empty means
// MUX_MCP_SERVERS is not injected; the proxy defaults to exposing all servers.
// ExtractRefs enables proxy-side identifier extraction (--extract-refs).
type MCPConfig struct {
	Servers     []string `yaml:"servers" json:"servers,omitempty"`
	ExtractRefs *bool    `yaml:"extract_refs,omitempty" json:"extract_refs,omitempty"`
}

type Project struct {
	ID            string        `yaml:"id" json:"id"`
	Name          string        `yaml:"name" json:"name"`
	RepoRoot      string        `yaml:"repo_root" json:"repo_root"`
	TrackingRoot  string        `yaml:"tracking_root" json:"tracking_root"`
	KnowledgeBase []string      `yaml:"knowledge_base" json:"knowledge_base,omitempty"`
	BootFragments []string      `yaml:"boot_fragments" json:"boot_fragments,omitempty"`
	Workspace     WorkspaceSpec `yaml:"workspace" json:"workspace"`
	MCP           MCPConfig     `yaml:"mcp" json:"mcp,omitempty"`
}

type WorkspaceSpec struct {
	DefaultMode  string `yaml:"default_mode" json:"default_mode"`
	WorktreeBase string `yaml:"worktree_base" json:"worktree_base,omitempty"`
	SessionRoot  string `yaml:"session_root" json:"session_root,omitempty"`
}

type Agent struct {
	ID            string           `yaml:"id" json:"id"`
	Name          string           `yaml:"name" json:"name"`
	Roles         []string         `yaml:"roles" json:"roles,omitempty"`
	Skills        []string         `yaml:"skills" json:"skills,omitempty"`
	ContextFiles  []string         `yaml:"context_files" json:"context_files,omitempty"`
	BootFragments []string         `yaml:"boot_fragments" json:"boot_fragments,omitempty"`
	Permissions   AgentPermissions `yaml:"permissions" json:"permissions"`

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
}

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
