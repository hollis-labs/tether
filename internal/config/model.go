package config

import (
	"context"
	"fmt"
	goauth "github.com/hollis-labs/libs/security/auth"
	"strings"
	"time"

	"github.com/hollis-labs/libs/core/util/apppaths"
	"github.com/hollis-labs/substrate/harness/sandbox"

	"github.com/hollis-labs/tether/internal/federation"
	"github.com/hollis-labs/tether/internal/launchprofile"
	"github.com/hollis-labs/tether/internal/mcpgateway"
)

type Global struct {
	Role        string            `yaml:"role,omitempty"`
	Modules     map[string]bool   `yaml:"modules,omitempty"`
	Environment EnvironmentConfig `yaml:"environment,omitempty"`
	Teams       TeamsConfig       `yaml:"teams"`
	MCP         mcpgateway.Config `yaml:"mcp" json:"mcp,omitempty"`
	Version     string            `yaml:"version"`
	Catalog     CatalogRoots      `yaml:"catalog"`
	Daemon      DaemonConfig      `yaml:"daemon"`
	AI          AIConfig          `yaml:"ai"`
	Identity    IdentityConfig    `yaml:"identity"`
	// Federation is the authority-routing messaging block. Its zero value
	// (enabled: false) is a standalone install — no peers, no routing,
	// behavior identical to pre-federation Tether. See internal/federation
	// and docs/messaging-federation.md.
	Federation federation.Config `yaml:"federation"`
}

// TeamsConfig controls optional team surfaces and host activation.
// Omission keeps teams disabled.
type TeamsConfig struct {
	Enabled bool `yaml:"enabled"`
}

// IdentityConfig rolls out verified attribution before authorization policy.
type IdentityConfig struct {
	Mode string `yaml:"mode"`
	// Points overrides Mode for one enforcement point (see IdentityPoints).
	// An omitted point follows Mode. Applied at daemon start only.
	Points map[string]string `yaml:"points,omitempty"`
	// Issuer is the iss claim of tokens Tether issues (POST /auth/token) and
	// the value verifiers in other apps must be configured with. Default "tether".
	Issuer string `yaml:"issuer,omitempty"`
	// MCPGrants is operator-provisioned authority for service/interactive
	// principals. Omission grants no upstreams, never the operator's inventory.
	MCPGrants map[string]PrincipalMCPGrant `yaml:"mcp_grants,omitempty"`
}

// IdentityPoints are the enforcement points whose mode can be set
// independently of identity.mode. Every other consumer follows identity.mode.
var IdentityPoints = []string{"local_api", "mcp_gateway", "teams"}

type PrincipalMCPGrant struct {
	Servers []string `yaml:"servers" json:"servers"`
}

func (c IdentityConfig) EffectiveMode() string {
	mode := normalizeIdentityMode(c.Mode)
	// HOLLIS_AUTH_TETHER, then HOLLIS_AUTH, override the configured global mode
	// (one setting name across apps). They only affect processes started with
	// them set; an unparsable value is ignored here and rejected by Validate.
	if env, err := goauth.ModeFromEnv("tether", ""); err == nil && env != "" {
		mode = normalizeIdentityMode(string(env))
	}
	if mode == "" {
		return "observe"
	}
	return mode
}

// normalizeIdentityMode lower-cases mode and canonicalizes the permissive
// alias to Tether's stored spelling, observe.
func normalizeIdentityMode(mode string) string {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "permissive" {
		return "observe"
	}
	return mode
}

// ModeFor is the effective mode of one enforcement point: its identity.points
// override when set, otherwise identity.mode.
func (c IdentityConfig) ModeFor(point string) string {
	if m := normalizeIdentityMode(c.Points[point]); m != "" {
		return m
	}
	return c.EffectiveMode()
}

// AIConfig controls the in-process AI gateway surface. The first schema slice
// is intentionally narrow: provider entries plus a default ordered route list.
type AIConfig struct {
	Providers []AIProviderConfig `yaml:"providers"`
	Policy    AIPolicyConfig     `yaml:"policy"`
	Routing   AIRoutingConfig    `yaml:"routing"`
}

type AIUsageBudgetPolicyConfig struct {
	MaxCostUSD *float64 `yaml:"max_cost_usd"`
	Window     string   `yaml:"window"`
	Scope      string   `yaml:"scope"`
}

type AIPolicyConfig struct {
	AllowReasoning   *bool                     `yaml:"allow_reasoning"`
	AllowTools       *bool                     `yaml:"allow_tools"`
	AllowAttachments *bool                     `yaml:"allow_attachments"`
	MaxOutputTokens  *int                      `yaml:"max_output_tokens"`
	MaxCostUSD       *float64                  `yaml:"max_cost_usd"`
	UsageBudget      AIUsageBudgetPolicyConfig `yaml:"usage_budget"`
}

type AIProviderConfig struct {
	ID                   string              `yaml:"id"`
	Type                 string              `yaml:"type"`
	CatalogProvider      string              `yaml:"catalog_provider,omitempty"`
	Price                string              `yaml:"price,omitempty"`
	Wire                 string              `yaml:"wire,omitempty"`
	Model                string              `yaml:"model"`
	Models               []string            `yaml:"models"`
	DefaultModel         string              `yaml:"default_model"`
	SecretRef            string              `yaml:"secret_ref"`
	BaseURL              string              `yaml:"base_url"`
	Enabled              bool                `yaml:"enabled"`
	IncludeReasoningText bool                `yaml:"include_reasoning_text,omitempty"`
	Policy               AIPolicyConfig      `yaml:"policy"`
	Extensions           *ProviderExtensions `yaml:"extensions,omitempty"`
	// DiscoverModels opts an openai/openai-compatible connection into a
	// bounded GET {base_url}/models at daemon start. Discovered ids are
	// appended after the configured models; configured ids keep precedence
	// and a failed discovery leaves the configured list unchanged.
	DiscoverModels bool `yaml:"discover_models,omitempty"`
	// APIVersion overrides the API version path segment for gemini providers
	// (default v1beta), for gateways that serve Gemini under another one.
	APIVersion string `yaml:"api_version,omitempty"`
	// Headers are static, non-secret request headers sent on every call
	// (openai/openai-compatible only), for gateways that require a stable
	// client or session identifier. Credentials belong in secret_ref, never
	// here: authorization-style header names are rejected.
	Headers map[string]string `yaml:"headers,omitempty"`
	// Capabilities declares what this provider's configured models support, for
	// models the catalog does not know (local servers, custom gateways). It
	// only applies to a model with no catalog entry; a catalog entry wins.
	Capabilities *AICapabilitiesConfig `yaml:"capabilities,omitempty"`
	// InputPriceUSDPerMTok is a decision provider's flat price per million
	// input tokens (output is free). Only the jev type uses it: JEV has no
	// models.dev entry, so the config declares the price.
	InputPriceUSDPerMTok *float64 `yaml:"input_price_usd_per_mtok,omitempty"`
	// TimeoutSeconds bounds one decision-provider attempt (jev). Zero uses the
	// adapter default.
	TimeoutSeconds int `yaml:"timeout_seconds,omitempty"`
}

// AICapabilitiesConfig mirrors the catalog capability flags a route is
// checked against: tool calling, reasoning and attachments.
type AICapabilitiesConfig struct {
	ToolCall   bool `yaml:"tool_call"`
	Reasoning  bool `yaml:"reasoning"`
	Attachment bool `yaml:"attachment"`
}

type ProviderExtensions struct {
	OpenRouter  *OpenRouterExtension  `yaml:"openrouter,omitempty"`
	HuggingFace *HuggingFaceExtension `yaml:"huggingface,omitempty"`
}

type OpenRouterExtension struct {
	Provider map[string]any `yaml:"provider,omitempty"`
}

type HuggingFaceExtension struct {
	BillTo string `yaml:"bill_to,omitempty"`
}

type AIRoutingConfig struct {
	DefaultProviderOrder []string        `yaml:"default_provider_order"`
	Routes               []AIRouteConfig `yaml:"routes"`
}

type AIRouteConfig struct {
	Provider          string                    `yaml:"provider"`
	Model             string                    `yaml:"model"`
	Mode              string                    `yaml:"mode"`
	Intent            string                    `yaml:"intent"`
	RequiresReasoning bool                      `yaml:"requires_reasoning"`
	RequiresTools     bool                      `yaml:"requires_tools"`
	AllowReasoning    *bool                     `yaml:"allow_reasoning"`
	AllowTools        *bool                     `yaml:"allow_tools"`
	AllowAttachments  *bool                     `yaml:"allow_attachments"`
	MaxOutputTokens   *int                      `yaml:"max_output_tokens"`
	MaxCostUSD        *float64                  `yaml:"max_cost_usd"`
	UsageBudget       AIUsageBudgetPolicyConfig `yaml:"usage_budget"`
}

func (p AIProviderConfig) EffectiveModels() []string {
	if len(p.Models) > 0 {
		return append([]string(nil), p.Models...)
	}
	if p.Model == "" {
		return nil
	}
	return []string{p.Model}
}

func (p AIProviderConfig) EffectiveDefaultModel() string {
	if p.DefaultModel != "" {
		return p.DefaultModel
	}
	if p.Model != "" {
		return p.Model
	}
	if len(p.Models) > 0 {
		return p.Models[0]
	}
	return ""
}

// DaemonConfig controls the long-lived tetherd process. See ADR 0002 for the
// scheme-prefixed listen_addr rationale. All path fields accept ~ expansion.
type DaemonConfig struct {
	// RemoteListener adds an enforce-only loopback TCP listener beside listen_addr.
	RemoteListener RemoteListenerConfig `yaml:"remote_listener,omitempty"`
	// MCPEndpoint is opt-in; startup/doctor validate its listener when enabled.
	MCPEndpoint MCPEndpointConfig `yaml:"mcp_endpoint,omitempty"`
	// MCPUpstreams selects planting for new launches; omitted means legacy_proxy.
	MCPUpstreams        string `yaml:"mcp_upstream_ownership,omitempty"`
	mcpOwnershipPresent bool
	mcpOwnershipInvalid bool
	// ListenAddr accepts "unix:/path" or "tcp:host:port". If empty, defaults
	// to "unix:~/.tether/run/tetherd.sock".
	ListenAddr string `yaml:"listen_addr"`
	// PIDFile records the child process PID. Defaults to
	// "~/.tether/run/tetherd.pid".
	PIDFile string `yaml:"pid_file"`
	// ShutdownTimeout caps how long Shutdown waits for in-flight sessions to
	// reach a terminal state. Go duration string; defaults to "10s".
	ShutdownTimeout string `yaml:"shutdown_timeout"`
	// EventsRetention controls the hourly age-based sweep of events,
	// proxy_events, ai_events, identity_audit and terminal a2a_tasks.
	// Enabled by default for 90 days.
	EventsRetention EventsRetentionConfig `yaml:"events_retention,omitempty"`
}

type RemoteListenerConfig struct {
	Enabled        bool     `yaml:"enabled"`
	ListenAddr     string   `yaml:"listen_addr"`
	AllowedHosts   []string `yaml:"allowed_hosts,omitempty"`
	AllowedOrigins []string `yaml:"allowed_origins,omitempty"`
}

type MCPEndpointConfig struct {
	Enabled bool `yaml:"enabled"`
}

// EventsRetentionConfig is daemon.events_retention:
//
//	events_retention:
//	  enabled: true # default true
//	  days: 90      # window; unset means DefaultEventsRetentionDays
//
// Retention is time-based only (decision D-50): a row-count cap could drop a
// long-running session's recent history at an arbitrary point.
type EventsRetentionConfig struct {
	Enabled *bool `yaml:"enabled,omitempty"`
	// Days is the window in days. Unset means DefaultEventsRetentionDays;
	// 0 or negative disables the sweep even when Enabled.
	Days *int `yaml:"days,omitempty"`
}

// DefaultEventsRetentionDays is the window when events retention is enabled
// without days (decision D-50).
const DefaultEventsRetentionDays = 90

// Window returns how long events are kept, or 0 when retention is off.
func (c EventsRetentionConfig) Window() time.Duration {
	if c.Enabled != nil && !*c.Enabled {
		return 0
	}
	days := DefaultEventsRetentionDays
	if c.Days != nil {
		days = *c.Days
	}
	if days <= 0 {
		return 0
	}
	return time.Duration(days) * 24 * time.Hour
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

// ShimHostConfig applies only to opt-in shim-hosted provider launches.
type ShimHostConfig struct {
	// UnitPrefix changes only names of newly created transient units.
	UnitPrefix   string `yaml:"unit_prefix"`
	JournalBytes int64  `yaml:"journal_bytes"`
	SystemdUser  bool   `yaml:"systemd_user"`
}

func (c ShimHostConfig) EffectiveJournalBytes() int64 {
	if c.JournalBytes == 0 {
		return 256 << 20
	}
	return c.JournalBytes
}

type Defaults struct {
	ShimHost ShimHostConfig `yaml:"shim_host"`

	WorkspaceRoot string `yaml:"workspace_root"`
	StateDB       string `yaml:"state_db"`
	TempRoot      string `yaml:"temp_root"`
	// PermissionMode is the fleet-wide default permission posture for
	// launched agents: "bypass" (no tool-permission prompts) or "default"
	// (refused what needs approval). Empty resolves to "default". An
	// agent's permissions.permission_mode overrides this per-agent. See
	// config.EffectivePermissionMode and config.ProviderPosture.
	PermissionMode string `yaml:"permission_mode"`
	// LaunchEngine selects how the agentlaunch.LaunchPlan that feeds
	// launcher.Compile is produced: "catalog" (default — the legacy
	// bespoke static-catalog walk) or "spec" (the S5 LaunchSpec resolver,
	// internal/specresolve). Empty resolves to "catalog". The
	// TETHER_LAUNCH_ENGINE env var overrides this. See app.LaunchEngine.
	LaunchEngine string `yaml:"launch_engine"`

	// LaunchHost selects direct execution (default) or opt-in shim hosting.
	// TETHER_LAUNCH_HOST overrides this value for each launch.
	LaunchHost string `yaml:"launch_host"`
	// LaunchSpecsRoot is the LaunchSpec corpus directory the "spec" launch
	// engine reads from. Empty resolves to <home>/.tether/launch-specs
	// (specresolve.DefaultSpecsRoot). The TETHER_LAUNCH_SPECS_ROOT env var
	// overrides this. Path expansion (~) is applied.
	LaunchSpecsRoot string `yaml:"launch_specs_root"`
	// ExtractRefs is the fleet-wide default for proxy-side identifier extraction
	// (sets --extract-refs on the planted `tether mcp` server). Empty/false resolves
	// to false (off by default). Can be overridden per-project or per-launch via
	// mcp.extract_refs.
	ExtractRefs bool `yaml:"extract_refs,omitempty"`

	// LineageMaxDepth and LineageMaxChildren cap plain session lineage
	// (CW-20261014-0052). Zero means unlimited.
	LineageMaxDepth    int `yaml:"lineage_max_depth,omitempty"`
	LineageMaxChildren int `yaml:"lineage_max_children,omitempty"`
	// LineageMaxLaunchesPerWindow caps child launches under one parent within
	// LineageLaunchWindow (a Go duration, default 1m), counting ended children.
	// Zero means unlimited.
	LineageMaxLaunchesPerWindow int    `yaml:"lineage_max_launches_per_window,omitempty"`
	LineageLaunchWindow         string `yaml:"lineage_launch_window,omitempty"`
}

// Launch-configuration types relocated to internal/launchprofile.
type (
	// MCPConfig is relocated to internal/launchprofile.
	MCPConfig = launchprofile.MCPConfig
	// LaunchContext is relocated to internal/launchprofile.
	LaunchContext = launchprofile.LaunchContext
	// Project is a backward-compatible alias for LaunchContext.
	Project = LaunchContext
	// WorkspaceSpec is relocated to internal/launchprofile.
	WorkspaceSpec = launchprofile.WorkspaceSpec
	// LaunchProfile is relocated to internal/launchprofile.
	LaunchProfile = launchprofile.LaunchProfile
	// Agent is a backward-compatible alias for LaunchProfile.
	Agent = LaunchProfile
	// AgentPermissions is relocated to internal/launchprofile.
	AgentPermissions = launchprofile.AgentPermissions
	// ProviderOverride is relocated to internal/launchprofile.
	ProviderOverride = launchprofile.ProviderOverride
)

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
	Lifecycle *launchprofile.LifecyclePolicy `yaml:"lifecycle,omitempty" json:"lifecycle,omitempty"`
	Route     *launchprofile.Route           `yaml:"route,omitempty" json:"route,omitempty"`
	// Chat lists the launch in the operator chat roster (GET /chat/agents).
	// DisplayName is the roster label; empty falls back to the agent id.
	Chat bool `yaml:"chat,omitempty" json:"chat,omitempty"`
	// Enabled turns the launch off without removing its file: false blocks new
	// launches and chat sends, never a running session. Absent means enabled.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// Worker is the default worker label (or id) for sessions started from
	// this launch by API-driven paths such as chat send. Empty means no
	// default: those paths refuse to start a session rather than run on the hub.
	Worker      string          `yaml:"worker,omitempty" json:"worker,omitempty"`
	DisplayName string          `yaml:"display_name,omitempty" json:"display_name,omitempty"`
	ID          string          `yaml:"id" json:"id"`
	Project     string          `yaml:"project" json:"project"`
	Agent       string          `yaml:"agent" json:"agent"`
	Provider    string          `yaml:"provider" json:"provider"`
	Workspace   LaunchWorkspace `yaml:"workspace" json:"workspace"`
	Prompt      PromptSpec      `yaml:"prompt" json:"prompt"`
	Overrides   LaunchOverrides `yaml:"overrides" json:"overrides"`
	MCP         MCPConfig       `yaml:"mcp" json:"mcp,omitempty"`
	Injection   LaunchInjection `yaml:"injection" json:"injection,omitempty"`
}

// IsEnabled reports whether the launch may be started; absent means yes.
func (l Launch) IsEnabled() bool { return l.Enabled == nil || *l.Enabled }

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

// LaunchInjection declares native files and boot-dir overlay entries to plant
// into a launched session's workspace.
//
// SECURITY — PERSISTED AT REST, NON-SECRET ONLY. Every InjectedFile resolved
// from this struct (whether its content is inline or loaded from a Source
// path) is materialized into launch.Plan, which is persisted verbatim as JSON
// in the launch_plans table. Injected content is therefore at-rest data.
// NEVER place API keys, tokens, or other secrets in injection content/source.
// Secrets must flow through provider env passthrough/whitelist mode (parent
// env, never persisted) or an external api-key-helper/keychain — see
// ProviderEnv and docs/shared-launch-adoption-guide.md.
type LaunchInjection struct {
	NativeFiles    []InjectedFile `yaml:"native_files" json:"native_files,omitempty"`
	BootDirOverlay []InjectedFile `yaml:"boot_dir_overlay" json:"boot_dir_overlay,omitempty"`
}

// InjectedFile describes one file to plant into a launched session.
//
// SECURITY — PERSISTED AT REST, NON-SECRET ONLY. Both Content (inline) and the
// file read from Source are resolved into launch.Plan and persisted as JSON in
// the launch_plans table. Treat all injected content as non-secret. Routing a
// secret through Content or Source persists it at rest — use provider env
// passthrough/whitelist instead.
type InjectedFile struct {
	Kind    string `yaml:"kind" json:"kind,omitempty"`
	ID      string `yaml:"id" json:"id,omitempty"`
	RelPath string `yaml:"rel_path" json:"rel_path,omitempty"`
	Content string `yaml:"content" json:"content,omitempty"`
	Source  string `yaml:"source" json:"source,omitempty"`
	Mode    uint32 `yaml:"mode" json:"mode,omitempty"`
}

type Catalog struct {
	// MCPServerEnabled retains validation metadata only, never upstream credentials.
	MCPServerEnabled map[string]bool
	BootMCPGrants    map[string][]string
	Global           Global
	Projects         map[string]LaunchContext
	Agents           map[string]LaunchProfile
	Providers        map[string]Provider
	Launches         map[string]Launch
	SandboxProfiles  map[string]sandbox.Profile
	// Paths is the go-apppaths Layout resolved by Load. It supplies the
	// FALLBACK storage paths (state_db, workspace_root, temp_root) for the
	// case where global.yaml omits the corresponding catalog default. The
	// explicit catalog value, when set, always wins — see
	// ResolveStateDB / ResolveWorkspaceRoot / ResolveTempRoot.
	Paths paths.Layout
}

var _ launchprofile.Source = (*Catalog)(nil)

// GetProfile satisfies launchprofile.Source by retrieving a launch profile from the catalog.
// If id matches a legacy launch entry in Launches, it resolves the agent profile and inherits
// the launch's provider.
func (c *Catalog) GetProfile(_ context.Context, id string) (*launchprofile.LaunchProfile, error) {
	if c == nil {
		return nil, launchprofile.ErrProfileNotFound
	}
	if l, ok := c.Launches[id]; ok {
		agent, aok := c.Agents[l.Agent]
		if !aok {
			agent = launchprofile.LaunchProfile{ID: l.Agent}
		}
		p := agent
		if l.Provider != "" {
			p.Provider = l.Provider
		}
		return &p, nil
	}
	if a, ok := c.Agents[id]; ok {
		cp := a
		return &cp, nil
	}
	return nil, fmt.Errorf("%w: %q", launchprofile.ErrProfileNotFound, id)
}

// GetContext satisfies launchprofile.Source by retrieving a launch context from the catalog.
// If id matches a legacy launch entry in Launches, it resolves the associated project context.
func (c *Catalog) GetContext(_ context.Context, id string) (*launchprofile.LaunchContext, error) {
	if c == nil {
		return nil, launchprofile.ErrContextNotFound
	}
	if l, ok := c.Launches[id]; ok {
		if proj, pok := c.Projects[l.Project]; pok {
			cp := proj
			return &cp, nil
		}
	}
	if p, ok := c.Projects[id]; ok {
		cp := p
		return &cp, nil
	}
	return nil, fmt.Errorf("%w: %q", launchprofile.ErrContextNotFound, id)
}
