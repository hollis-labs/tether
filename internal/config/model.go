package config

import "github.com/hollis-labs/go-sandbox/sandbox"

type Global struct {
	Version string       `yaml:"version"`
	Catalog CatalogRoots `yaml:"catalog"`
	Daemon  DaemonConfig `yaml:"daemon"`
}

// DaemonConfig controls the long-lived muxd process. See ADR 0002 for the
// scheme-prefixed listen_addr rationale. All path fields accept ~ expansion.
type DaemonConfig struct {
	// ListenAddr accepts "unix:/path" or "tcp:host:port". If empty, defaults
	// to "unix:~/.agent-mux/run/muxd.sock".
	ListenAddr string `yaml:"listen_addr"`
	// PIDFile records the child process PID. Defaults to
	// "~/.agent-mux/run/muxd.pid".
	PIDFile string `yaml:"pid_file"`
	// ShutdownTimeout caps how long Shutdown waits for in-flight sessions to
	// reach a terminal state. Go duration string; defaults to "10s".
	ShutdownTimeout string `yaml:"shutdown_timeout"`
}

type CatalogRoots struct {
	Roots    CatalogPaths `yaml:"roots"`
	Defaults Defaults     `yaml:"defaults"`
}

type CatalogPaths struct {
	Projects  string `yaml:"projects"`
	Agents    string `yaml:"agents"`
	Providers string `yaml:"providers"`
	Launches  string `yaml:"launches"`
	Boot      string `yaml:"boot"`
}

type Defaults struct {
	WorkspaceRoot string `yaml:"workspace_root"`
	StateDB       string `yaml:"state_db"`
	TempRoot      string `yaml:"temp_root"`
}

// MCPConfig holds per-project or per-launch MCP proxy settings that are
// injected into agent sessions at launch time. Servers lists the upstream MCP
// server IDs to expose as native tools (sets MUX_MCP_SERVERS). Empty means
// MUX_MCP_SERVERS is not injected; the proxy defaults to exposing all servers.
type MCPConfig struct {
	Servers []string `yaml:"servers" json:"servers,omitempty"`
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
}

// ProviderOverride carries per-provider customization for an Agent. Fields are
// additive — start narrow and expand when concrete needs surface.
type ProviderOverride struct {
	ExtraArgs []string          `yaml:"extra_args" json:"extra_args,omitempty"`
	Env       map[string]string `yaml:"env" json:"env,omitempty"`
}

type Provider struct {
	ID   string `yaml:"id" json:"id"`
	Type string `yaml:"type" json:"type"`
	// Provider is the product/adapter brand ("claude", "codex",
	// "opencode"). RuntimeKind selects the transport/lifecycle.
	Provider    string   `yaml:"provider" json:"provider,omitempty"`
	RuntimeKind string   `yaml:"runtime_kind" json:"runtime_kind,omitempty"`
	Command     string   `yaml:"command" json:"command"`
	Args        []string `yaml:"args" json:"args,omitempty"`
	// Adapter names the go-providers CLIAdapter to use when Type is
	// "cli-goprovider". Valid values: "claude", "codex".
	// Command is optional — if empty, adapter.Detect() resolves the binary.
	Adapter   string        `yaml:"adapter" json:"adapter,omitempty"`
	Bootstrap BootstrapSpec `yaml:"bootstrap" json:"bootstrap"`
	Env       ProviderEnv   `yaml:"env" json:"env"`
}

type BootstrapSpec struct {
	Mode         string `yaml:"mode" json:"mode,omitempty"`
	PromptPrefix string `yaml:"prompt_prefix" json:"prompt_prefix,omitempty"`
}

// ProviderEnv describes how the provider composes a child process's
// environment. See internal/provider/env.go for full semantics.
//
// Mode is "merge" (default) or "whitelist". Merge inherits the parent
// environment, drops any keys in Redact, and overlays explicit overrides.
// Whitelist starts empty and copies only the keys listed in Passthrough.
//
// Passthrough is consulted only in whitelist mode; Redact is consulted only
// in merge mode. Both fields are preserved across modes so catalog authors
// can flip mode without rewriting the rest of the block.
type ProviderEnv struct {
	Mode        string   `yaml:"mode" json:"mode"`
	Passthrough []string `yaml:"passthrough" json:"passthrough,omitempty"`
	Redact      []string `yaml:"redact" json:"redact,omitempty"`
}

type Launch struct {
	ID        string          `yaml:"id" json:"id"`
	Project   string          `yaml:"project" json:"project"`
	Agent     string          `yaml:"agent" json:"agent"`
	Provider  string          `yaml:"provider" json:"provider"`
	Workspace LaunchWorkspace `yaml:"workspace" json:"workspace"`
	Prompt    PromptSpec      `yaml:"prompt" json:"prompt"`
	Overrides LaunchOverrides `yaml:"overrides" json:"overrides"`
	MCP       MCPConfig       `yaml:"mcp" json:"mcp,omitempty"`
	Injection LaunchInjection `yaml:"injection" json:"injection,omitempty"`
}

type LaunchWorkspace struct {
	Mode         string `yaml:"mode" json:"mode"`
	WorktreeName string `yaml:"worktree_name" json:"worktree_name,omitempty"`
	WriteHome    string `yaml:"write_home" json:"write_home,omitempty"`
}

type PromptSpec struct {
	IncludeProjectBoot   bool `yaml:"include_project_boot" json:"include_project_boot"`
	IncludeAgentBoot     bool `yaml:"include_agent_boot" json:"include_agent_boot"`
	IncludeKnowledgeBase bool `yaml:"include_knowledge_base" json:"include_knowledge_base"`
}

type LaunchOverrides struct {
	Env map[string]string `yaml:"env" json:"env,omitempty"`
}

type LaunchInjection struct {
	NativeFiles    []InjectedFile `yaml:"native_files" json:"native_files,omitempty"`
	BootDirOverlay []InjectedFile `yaml:"boot_dir_overlay" json:"boot_dir_overlay,omitempty"`
}

type InjectedFile struct {
	Kind    string `yaml:"kind" json:"kind,omitempty"`
	ID      string `yaml:"id" json:"id,omitempty"`
	RelPath string `yaml:"rel_path" json:"rel_path,omitempty"`
	Content string `yaml:"content" json:"content,omitempty"`
	Source  string `yaml:"source" json:"source,omitempty"`
	Mode    uint32 `yaml:"mode" json:"mode,omitempty"`
}

type Catalog struct {
	Global          Global
	Projects        map[string]Project
	Agents          map[string]Agent
	Providers       map[string]Provider
	Launches        map[string]Launch
	SandboxProfiles map[string]sandbox.Profile
}
