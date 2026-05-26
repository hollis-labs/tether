// Command tether_sysop serves the TetherSysop Sysop UI.
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/hollis-labs/go-messaging"
	"github.com/hollis-labs/go-modelsdev/modelsdev"
	"github.com/hollis-labs/tether/apps/sysop/internal/webui"
	"github.com/hollis-labs/tether/internal/agent"
	tetherapi "github.com/hollis-labs/tether/internal/api"
	muxclient "github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/events"
	launchplan "github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/mcpadapter"
	"github.com/hollis-labs/tether/internal/modelcatalog"
	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/store"
	"gopkg.in/yaml.v3"
)

// errStateDBUnset signals that the catalog does not configure a state DB
// path. Handlers treat it as "no data yet" rather than a hard error.
var errStateDBUnset = errors.New("state db not configured")

const secretPreserveMarker = "__PRESERVE__"
const secretDeleteMarker = "__DELETE__"

const (
	kindSysopSessionStop        = "sysop.session_stop"
	kindSysopSessionCheckpoint  = "sysop.session_checkpoint"
	kindSysopLogicalResume      = "sysop.logical_agent_resume"
	kindSysopSessionCleanup     = "sysop.session_cleanup"
	kindSysopLogicalAgentPolicy = "sysop.logical_agent_policy"
)

type appServer struct {
	catalogRoot string
	addr        string
	startedAt   time.Time
}

type healthResponse struct {
	Status      string `json:"status"`
	CatalogRoot string `json:"catalog_root"`
	Error       string `json:"error,omitempty"`
}

type settingsResponse struct {
	Server    settingsServerDTO     `json:"server"`
	Paths     settingsPathsDTO      `json:"paths"`
	Daemon    settingsDaemonDTO     `json:"daemon"`
	Catalog   settingsCatalogDTO    `json:"catalog"`
	MCP       settingsMCPDTO        `json:"mcp"`
	Providers []settingsProviderDTO `json:"providers"`
	Launches  settingsLaunchesDTO   `json:"launches"`
	Sessions  settingsSessionsDTO   `json:"sessions"`
	Roadmap   []settingsRoadmapDTO  `json:"roadmap"`
	Error     string                `json:"error,omitempty"`
}

type aiSettingsResponse struct {
	Config  aiConfigDTO        `json:"config"`
	Runtime aiRuntimeStatusDTO `json:"runtime"`
	Error   string             `json:"error,omitempty"`
}

type aiRuntimeStatusDTO struct {
	DaemonReachable bool   `json:"daemon_reachable"`
	Providers       int    `json:"providers"`
	Models          int    `json:"models"`
	Routes          int    `json:"routes"`
	LastError       string `json:"last_error,omitempty"`
}

type aiRuntimeResponse struct {
	Providers []tetherapi.AIProviderDTO `json:"providers"`
	Models    []tetherapi.AIModelDTO    `json:"models"`
	Routes    []tetherapi.AIRouteDTO    `json:"routes"`
	Error     string                    `json:"error,omitempty"`
}

type aiUsageResponse struct {
	Summary tetherapi.AIUsageResponse `json:"summary"`
	Error   string                    `json:"error,omitempty"`
}

type aiAuditResponse struct {
	Events []tetherapi.AIAuditEventDTO `json:"events"`
	Count  int                         `json:"count"`
	Error  string                      `json:"error,omitempty"`
}

type aiBudgetsResponse struct {
	Budgets []tetherapi.AIUsageBudgetEntryDTO `json:"budgets"`
	Count   int                               `json:"count"`
	Error   string                            `json:"error,omitempty"`
}

type aiCatalogModelsResponse struct {
	ProviderType     string              `json:"provider_type"`
	VendorProviderID string              `json:"vendor_provider_id,omitempty"`
	VendorProvider   string              `json:"vendor_provider_name,omitempty"`
	Models           []aiCatalogModelDTO `json:"models"`
	LastFetchedAt    string              `json:"last_fetched_at,omitempty"`
	FromCacheOnly    bool                `json:"from_cache_only,omitempty"`
	Error            string              `json:"error,omitempty"`
}

type aiCatalogModelDTO struct {
	ID                  string   `json:"id"`
	Name                string   `json:"name,omitempty"`
	Family              string   `json:"family,omitempty"`
	ContextWindow       int      `json:"context_window,omitempty"`
	MaxOutputTokens     int      `json:"max_output_tokens,omitempty"`
	InputModalities     []string `json:"input_modalities,omitempty"`
	OutputModalities    []string `json:"output_modalities,omitempty"`
	SupportsTools       bool     `json:"supports_tools,omitempty"`
	SupportsReasoning   bool     `json:"supports_reasoning,omitempty"`
	SupportsAttachments bool     `json:"supports_attachments,omitempty"`
	InputCostUSD        float64  `json:"input_cost_usd_per_mtok,omitempty"`
	OutputCostUSD       float64  `json:"output_cost_usd_per_mtok,omitempty"`
}

type aiConfigDTO struct {
	Policy               aiPolicyDTO     `json:"policy"`
	DefaultProviderOrder []string        `json:"default_provider_order,omitempty"`
	Providers            []aiProviderDTO `json:"providers"`
	Routes               []aiRouteDTO    `json:"routes"`
}

type aiUsageBudgetDTO struct {
	MaxCostUSD *float64 `json:"max_cost_usd,omitempty"`
	Window     string   `json:"window,omitempty"`
	Scope      string   `json:"scope,omitempty"`
}

type aiPolicyDTO struct {
	AllowReasoning   *bool            `json:"allow_reasoning,omitempty"`
	AllowTools       *bool            `json:"allow_tools,omitempty"`
	AllowAttachments *bool            `json:"allow_attachments,omitempty"`
	MaxOutputTokens  *int             `json:"max_output_tokens,omitempty"`
	MaxCostUSD       *float64         `json:"max_cost_usd,omitempty"`
	UsageBudget      aiUsageBudgetDTO `json:"usage_budget,omitempty"`
}

type aiProviderDTO struct {
	ID           string      `json:"id"`
	Type         string      `json:"type"`
	Model        string      `json:"model,omitempty"`
	Models       []string    `json:"models,omitempty"`
	DefaultModel string      `json:"default_model,omitempty"`
	SecretRef    string      `json:"secret_ref,omitempty"`
	BaseURL      string      `json:"base_url,omitempty"`
	Enabled      bool        `json:"enabled"`
	Policy       aiPolicyDTO `json:"policy"`
}

type aiRouteDTO struct {
	Provider          string      `json:"provider"`
	Model             string      `json:"model"`
	Mode              string      `json:"mode,omitempty"`
	Intent            string      `json:"intent,omitempty"`
	RequiresReasoning bool        `json:"requires_reasoning,omitempty"`
	RequiresTools     bool        `json:"requires_tools,omitempty"`
	Policy            aiPolicyDTO `json:"policy"`
}

type settingsServerDTO struct {
	HTTPAddr    string `json:"http_addr"`
	CatalogRoot string `json:"catalog_root"`
	PID         int    `json:"pid"`
	StartedAt   string `json:"started_at"`
	UptimeSec   int64  `json:"uptime_sec"`
}

type settingsPathDTO struct {
	Path   string `json:"path"`
	Exists bool   `json:"exists"`
}

type settingsPathsDTO struct {
	CatalogRoot     settingsPathDTO `json:"catalog_root"`
	StateDB         settingsPathDTO `json:"state_db"`
	WorkspaceRoot   settingsPathDTO `json:"workspace_root"`
	TempRoot        settingsPathDTO `json:"temp_root"`
	LaunchSpecsRoot settingsPathDTO `json:"launch_specs_root"`
	ProjectsRoot    settingsPathDTO `json:"projects_root"`
	AgentsRoot      settingsPathDTO `json:"agents_root"`
	ProvidersRoot   settingsPathDTO `json:"providers_root"`
	LaunchesRoot    settingsPathDTO `json:"launches_root"`
	BootRoot        settingsPathDTO `json:"boot_root"`
	MCPServersRoot  settingsPathDTO `json:"mcp_servers_root"`
}

type settingsDaemonDTO struct {
	ListenAddr       string `json:"listen_addr"`
	ListenKind       string `json:"listen_kind"`
	ListenEndpoint   string `json:"listen_endpoint"`
	SocketExists     bool   `json:"socket_exists"`
	PIDFile          string `json:"pid_file"`
	PIDFileExists    bool   `json:"pid_file_exists"`
	PID              int    `json:"pid,omitempty"`
	PIDRunning       bool   `json:"pid_running"`
	ShutdownTimeout  string `json:"shutdown_timeout"`
	PermissionMode   string `json:"permission_mode"`
	LaunchEngine     string `json:"launch_engine"`
	LaunchSpecsRoot  string `json:"launch_specs_root"`
	ConfigModifiedAt string `json:"config_modified_at,omitempty"`
	RestartRequired  bool   `json:"restart_required"`
}

type settingsCatalogDTO struct {
	Version   string `json:"version"`
	Projects  int    `json:"projects"`
	Agents    int    `json:"agents"`
	Providers int    `json:"providers"`
	Launches  int    `json:"launches"`
}

type settingsMCPDTO struct {
	Servers          int      `json:"servers"`
	Enabled          int      `json:"enabled"`
	WithTokens       int      `json:"with_tokens"`
	Transports       []string `json:"transports"`
	Root             string   `json:"root"`
	RootExists       bool     `json:"root_exists"`
	ConfigSurface    string   `json:"config_surface"`
	ConfigModifiedAt string   `json:"config_modified_at,omitempty"`
	RestartRequired  bool     `json:"restart_required"`
}

type settingsProviderDTO struct {
	ID                 string   `json:"id"`
	Type               string   `json:"type"`
	Provider           string   `json:"provider"`
	RuntimeKind        string   `json:"runtime_kind"`
	Command            string   `json:"command"`
	Args               []string `json:"args,omitempty"`
	Adapter            string   `json:"adapter,omitempty"`
	BootstrapMode      string   `json:"bootstrap_mode"`
	BootstrapPrefix    string   `json:"bootstrap_prefix,omitempty"`
	EnvMode            string   `json:"env_mode"`
	EnvPassthrough     []string `json:"env_passthrough,omitempty"`
	EnvRedact          []string `json:"env_redact,omitempty"`
	ReferencedLaunches int      `json:"referenced_launches"`
}

type settingsLaunchesDTO struct {
	Total         int `json:"total"`
	WithMCP       int `json:"with_mcp"`
	WithInjection int `json:"with_injection"`
	WithEnv       int `json:"with_env"`
	WithWorktree  int `json:"with_worktree"`
}

type settingsSessionsDTO struct {
	Total   int    `json:"total"`
	Running int    `json:"running"`
	Ended   int    `json:"ended"`
	Error   string `json:"error,omitempty"`
}

type settingsRoadmapDTO struct {
	Area   string `json:"area"`
	Status string `json:"status"`
	Next   string `json:"next"`
}

type catalogResponse struct {
	Projects  []projectDTO  `json:"projects"`
	Agents    []agentDTO    `json:"agents"`
	Providers []providerDTO `json:"providers"`
	Launches  []launchDTO   `json:"launches"`
}

type projectDTO struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	RepoRoot   string   `json:"repo_root"`
	Mode       string   `json:"mode"`
	MCPServers []string `json:"mcp_servers,omitempty"`
}

type agentDTO struct {
	ID     string   `json:"id"`
	Name   string   `json:"name"`
	Roles  []string `json:"roles,omitempty"`
	Skills []string `json:"skills,omitempty"`
}

type providerDTO struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	Provider    string `json:"provider"`
	RuntimeKind string `json:"runtime_kind"`
	Command     string `json:"command"`
}

type launchDTO struct {
	ID            string `json:"id"`
	Project       string `json:"project"`
	Agent         string `json:"agent"`
	Provider      string `json:"provider"`
	WorkspaceMode string `json:"workspace_mode"`
	NativeFiles   int    `json:"native_files"`
	BootOverlay   int    `json:"boot_overlay"`
	Profile       string `json:"profile,omitempty"`
	LaunchPlan    string `json:"launch_plan,omitempty"`
	PlanError     string `json:"plan_error,omitempty"`
}

type sessionsResponse struct {
	Sessions []sessionDTO `json:"sessions"`
	Total    int          `json:"total"`
	Running  int          `json:"running"`
	Ended    int          `json:"ended"`
	Error    string       `json:"error,omitempty"`
}

type launchActionRequest struct {
	LaunchID string `json:"launch_id"`
}

type launchSaveRequest struct {
	ID                   string                `json:"id"`
	Project              string                `json:"project"`
	Agent                string                `json:"agent"`
	Provider             string                `json:"provider"`
	WorkspaceMode        string                `json:"workspace_mode"`
	WorktreeName         string                `json:"worktree_name,omitempty"`
	IncludeProjectBoot   bool                  `json:"include_project_boot"`
	IncludeAgentBoot     bool                  `json:"include_agent_boot"`
	IncludeKnowledgeBase bool                  `json:"include_knowledge_base"`
	MCPServers           []string              `json:"mcp_servers,omitempty"`
	EnvOverrides         map[string]string     `json:"env_overrides,omitempty"`
	NativeFiles          []injectedFileRequest `json:"native_files,omitempty"`
	BootDirOverlay       []injectedFileRequest `json:"boot_dir_overlay,omitempty"`
}

type injectedFileRequest struct {
	Kind    string `json:"kind,omitempty"`
	ID      string `json:"id,omitempty"`
	RelPath string `json:"rel_path,omitempty"`
	Content string `json:"content,omitempty"`
	Source  string `json:"source,omitempty"`
	Mode    uint32 `json:"mode,omitempty"`
}

type launchPreviewResponse struct {
	Existing           bool     `json:"existing"`
	CurrentProfile     string   `json:"current_profile,omitempty"`
	NextProfile        string   `json:"next_profile,omitempty"`
	CurrentPlan        string   `json:"current_plan,omitempty"`
	NextPlan           string   `json:"next_plan,omitempty"`
	CurrentProfileYAML string   `json:"current_profile_yaml,omitempty"`
	NextProfileYAML    string   `json:"next_profile_yaml,omitempty"`
	ProfileDiff        string   `json:"profile_diff,omitempty"`
	PlanDiff           string   `json:"plan_diff,omitempty"`
	ProfileChanged     bool     `json:"profile_changed"`
	PlanChanged        bool     `json:"plan_changed"`
	TargetPath         string   `json:"target_path,omitempty"`
	WillCreateBackup   bool     `json:"will_create_backup"`
	CommentLossRisk    bool     `json:"comment_loss_risk"`
	CurrentPlanError   string   `json:"current_plan_error,omitempty"`
	NextPlanError      string   `json:"next_plan_error,omitempty"`
	Warnings           []string `json:"warnings,omitempty"`
	Error              string   `json:"error,omitempty"`
}

type sessionActionRequest struct {
	ID string `json:"id"`
}

type sessionTextActionRequest struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

type sessionResizeActionRequest struct {
	ID   string `json:"id"`
	Rows uint16 `json:"rows"`
	Cols uint16 `json:"cols"`
}

type checkpointActionRequest struct {
	ID      string `json:"id"`
	Status  string `json:"status,omitempty"`
	Summary string `json:"summary,omitempty"`
}

type resumeActionRequest struct {
	LogicalAgentID string `json:"logical_agent_id"`
}

type logicalAgentPolicySaveRequest struct {
	LogicalAgentID   string `json:"logical_agent_id"`
	CheckpointPolicy string `json:"checkpoint_policy"`
	CheckpointStatus string `json:"checkpoint_status,omitempty"`
}

type logicalAgentPolicyResponse struct {
	LogicalAgentID   string `json:"logical_agent_id"`
	Name             string `json:"name,omitempty"`
	LaunchID         string `json:"launch_id,omitempty"`
	CheckpointPolicy string `json:"checkpoint_policy"`
	CheckpointStatus string `json:"checkpoint_status,omitempty"`
	UpdatedAt        string `json:"updated_at,omitempty"`
	Error            string `json:"error,omitempty"`
}

type sessionCleanupRequest struct {
	OlderThanDays int  `json:"older_than_days"`
	Limit         int  `json:"limit"`
	DryRun        bool `json:"dry_run"`
}

type globalSettingsSaveRequest struct {
	ShutdownTimeout string `json:"shutdown_timeout"`
	PermissionMode  string `json:"permission_mode"`
	LaunchEngine    string `json:"launch_engine"`
	LaunchSpecsRoot string `json:"launch_specs_root"`
	WorkspaceRoot   string `json:"workspace_root"`
	StateDB         string `json:"state_db"`
	TempRoot        string `json:"temp_root"`
}

type aiSettingsSaveRequest struct {
	Config aiConfigDTO `json:"config"`
}

type systemResourceActionRequest struct {
	Resource string `json:"resource"`
	Action   string `json:"action"`
}

type actionResponse struct {
	Status         string `json:"status"`
	SessionID      string `json:"session_id,omitempty"`
	Workspace      string `json:"workspace,omitempty"`
	Log            string `json:"log,omitempty"`
	ProviderID     string `json:"provider_id,omitempty"`
	ProviderKind   string `json:"provider_kind,omitempty"`
	LogicalAgentID string `json:"logical_agent_id,omitempty"`
	ExitCode       *int   `json:"exit_code,omitempty"`
	Count          int    `json:"count,omitempty"`
	Output         string `json:"output,omitempty"`
	BackupPath     string `json:"backup_path,omitempty"`
	Error          string `json:"error,omitempty"`
}

type brokerEnvelopeDTO struct {
	ID            string `json:"id"`
	Sender        string `json:"sender,omitempty"`
	Recipient     string `json:"recipient,omitempty"`
	WorkflowID    string `json:"workflow_id,omitempty"`
	CorrelationID string `json:"correlation_id,omitempty"`
	MessageType   string `json:"message_type,omitempty"`
	Priority      int    `json:"priority"`
	Payload       string `json:"payload,omitempty"`
	CreatedAt     string `json:"created_at"`
	DeliveredAt   string `json:"delivered_at,omitempty"`
	ConsumedAt    string `json:"consumed_at,omitempty"`
	AuditJSON     string `json:"audit_json,omitempty"`
}

type brokerEnvelopesResponse struct {
	Envelopes []brokerEnvelopeDTO `json:"envelopes"`
	Error     string              `json:"error,omitempty"`
}

type mcpServerSaveRequest struct {
	ID        string            `json:"id"`
	Transport string            `json:"transport"`
	Command   string            `json:"command,omitempty"`
	Args      []string          `json:"args,omitempty"`
	URL       string            `json:"url,omitempty"`
	Token     string            `json:"token,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	Scopes    []string          `json:"scopes,omitempty"`
	Tags      []string          `json:"tags,omitempty"`
	Enabled   *bool             `json:"enabled,omitempty"`
}

type mcpServerIDRequest struct {
	ID      string `json:"id"`
	Enabled *bool  `json:"enabled,omitempty"`
}

type providerSaveRequest struct {
	ID              string   `json:"id"`
	Type            string   `json:"type"`
	Provider        string   `json:"provider,omitempty"`
	RuntimeKind     string   `json:"runtime_kind,omitempty"`
	Command         string   `json:"command,omitempty"`
	Args            []string `json:"args,omitempty"`
	Adapter         string   `json:"adapter,omitempty"`
	BootstrapMode   string   `json:"bootstrap_mode,omitempty"`
	BootstrapPrefix string   `json:"bootstrap_prefix,omitempty"`
	EnvMode         string   `json:"env_mode,omitempty"`
	EnvPassthrough  []string `json:"env_passthrough,omitempty"`
	EnvRedact       []string `json:"env_redact,omitempty"`
}

type providerIDRequest struct {
	ID string `json:"id"`
}

type registryCollectionResponse struct {
	Rows  []registry.Profile `json:"rows"`
	Error string             `json:"error,omitempty"`
}

type registrySaveRequest struct {
	Kind          string             `json:"kind"`
	URN           string             `json:"urn,omitempty"`
	DisplayName   string             `json:"display_name"`
	Title         string             `json:"title,omitempty"`
	Role          string             `json:"role,omitempty"`
	Description   string             `json:"description,omitempty"`
	Avatar        string             `json:"avatar,omitempty"`
	Project       string             `json:"project,omitempty"`
	Status        string             `json:"status,omitempty"`
	HealthStatus  string             `json:"health_status,omitempty"`
	HostAddress   string             `json:"host_address,omitempty"`
	LastUpdatedBy string             `json:"last_updated_by,omitempty"`
	Callback      *registry.Callback `json:"callback,omitempty"`
	Capabilities  []string           `json:"capabilities,omitempty"`
	Skills        []registry.Skill   `json:"skills,omitempty"`
	Links         []registry.Link    `json:"links,omitempty"`
}

type registryURNRequest struct {
	URN string `json:"urn"`
}

type registryBootstrapRequest struct {
	Force     bool   `json:"force"`
	Substrate string `json:"substrate,omitempty"`
	WriteBack *bool  `json:"write_back,omitempty"`
}

type sessionDTO struct {
	ID             string `json:"id"`
	LaunchID       string `json:"launch_id"`
	ProjectID      string `json:"project_id"`
	LogicalAgentID string `json:"logical_agent_id"`
	ProviderID     string `json:"provider_id"`
	ProviderKind   string `json:"provider_kind"`
	Workspace      string `json:"workspace"`
	State          string `json:"state"`
	PID            *int   `json:"pid,omitempty"`
	ExitCode       *int   `json:"exit_code,omitempty"`
	CreatedAt      string `json:"created_at"`
	UpdatedAt      string `json:"updated_at"`
	EndedAt        string `json:"ended_at,omitempty"`
}

func main() {
	addr := flag.String("addr", ":8947", "HTTP listen address")
	catalogRoot := flag.String("catalog", "~/.tether/catalog", "Tether catalog root")
	flag.Parse()

	server := &appServer{catalogRoot: config.Expand(*catalogRoot), addr: *addr, startedAt: time.Now()}
	mux := http.NewServeMux()

	mux.HandleFunc("/api/health", server.handleHealth)
	mux.HandleFunc("/api/settings", server.handleSettings)
	mux.HandleFunc("/api/settings/global/save", server.handleGlobalSettingsSave)
	mux.HandleFunc("/api/settings/providers/save", server.handleProviderSave)
	mux.HandleFunc("/api/settings/providers/delete", server.handleProviderDelete)
	mux.HandleFunc("/api/ai/settings", server.handleAISettings)
	mux.HandleFunc("/api/ai/settings/save", server.handleAISettingsSave)
	mux.HandleFunc("/api/ai/catalog/models", server.handleAICatalogModels)
	mux.HandleFunc("/api/ai/runtime", server.handleAIRuntime)
	mux.HandleFunc("/api/ai/usage", server.handleAIUsage)
	mux.HandleFunc("/api/ai/audit", server.handleAIAudit)
	mux.HandleFunc("/api/ai/budgets", server.handleAIBudgets)
	mux.HandleFunc("/api/system/resource/action", server.handleSystemResourceAction)
	mux.HandleFunc("/api/overview", server.handleOverview)
	mux.HandleFunc("/api/catalog", server.handleCatalog)
	mux.HandleFunc("/api/launches/save", server.handleLaunchSave)
	mux.HandleFunc("/api/launches/preview", server.handleLaunchPreview)
	mux.HandleFunc("/api/launches/delete", server.handleLaunchDelete)
	mux.HandleFunc("/api/launches/launch", server.handleLaunchProfile)
	mux.HandleFunc("/api/sessions", server.handleSessions)
	mux.HandleFunc("/api/sessions/stop", server.handleSessionStop)
	mux.HandleFunc("/api/sessions/turn", server.handleSessionTurn)
	mux.HandleFunc("/api/sessions/input", server.handleSessionInput)
	mux.HandleFunc("/api/sessions/resize", server.handleSessionResize)
	mux.HandleFunc("/api/sessions/wait", server.handleSessionWait)
	mux.HandleFunc("/api/sessions/checkpoint", server.handleSessionCheckpoint)
	mux.HandleFunc("/api/logical-agents/resume", server.handleLogicalAgentResume)
	mux.HandleFunc("/api/logical-agents/policy", server.handleLogicalAgentPolicy)
	mux.HandleFunc("/api/sessions/attach", server.handleSessionAttach)
	mux.HandleFunc("/api/sessions/cleanup", server.handleSessionCleanup)
	mux.HandleFunc("/api/sessions/detail", server.handleSessionDetail)
	mux.HandleFunc("/api/messages", server.handleMessages)
	mux.HandleFunc("/api/messages/archive", server.handleMessageArchive)
	mux.HandleFunc("/api/messages/read", server.handleMessageMarkRead)
	mux.HandleFunc("/api/messages/groups", server.handleMessageGroups)
	mux.HandleFunc("/api/messages/groups/create", server.handleMessageGroupCreate)
	mux.HandleFunc("/api/messages/agents", server.handleMessageAgents)
	mux.HandleFunc("/api/broker/envelopes", server.handleBrokerEnvelopes)
	mux.HandleFunc("/api/activity/events", server.handleActivityEvents)
	mux.HandleFunc("/api/activity/tool-calls", server.handleActivityToolCalls)
	mux.HandleFunc("/api/mcp/servers", server.handleMCPServers)
	mux.HandleFunc("/api/mcp/servers/save", server.handleMCPServerSave)
	mux.HandleFunc("/api/mcp/servers/delete", server.handleMCPServerDelete)
	mux.HandleFunc("/api/mcp/servers/toggle", server.handleMCPServerToggle)
	mux.HandleFunc("/api/mcp/tools", server.handleMCPTools)
	mux.HandleFunc("/api/registry", server.handleRegistryList)
	mux.HandleFunc("/api/registry/save", server.handleRegistrySave)
	mux.HandleFunc("/api/registry/deregister", server.handleRegistryDeregister)
	mux.HandleFunc("/api/registry/sync", server.handleRegistrySync)
	mux.HandleFunc("/api/registry/bootstrap", server.handleRegistryBootstrap)

	// The Agent Ops UI — served from the embedded frontend build by go-webui.
	webui.Mount(mux)

	log.Printf("TetherSysop listening on http://localhost%s%s/", *addr, webui.BasePath)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Fatal(err)
	}
}

func (s *appServer) handleHealth(w http.ResponseWriter, _ *http.Request) {
	resp := healthResponse{Status: "ok", CatalogRoot: s.catalogRoot}
	if _, err := os.Stat(s.catalogRoot); err != nil {
		resp.Status = "degraded"
		resp.Error = err.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *appServer) handleSettings(w http.ResponseWriter, _ *http.Request) {
	cat, err := config.Load(s.catalogRoot)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, settingsResponse{
			Server: s.settingsServer(),
			Error:  err.Error(),
		})
		return
	}

	stateDB := config.Expand(cat.Global.Catalog.Defaults.StateDB)
	workspaceRoot := config.Expand(cat.Global.Catalog.Defaults.WorkspaceRoot)
	tempRoot := config.Expand(cat.Global.Catalog.Defaults.TempRoot)
	launchSpecsRoot := effectiveLaunchSpecsRoot(cat)
	mcpRoot := filepath.Join(s.catalogRoot, "mcp-servers")
	daemon := settingsDaemon(cat)
	globalMod := fileModTime(filepath.Join(s.catalogRoot, "global.yaml"))
	mcpMod := latestYAMLModTime(mcpRoot)
	daemonReference := fileModTime(daemon.PIDFile)
	configMod := maxTime(globalMod, mcpMod)
	if !configMod.IsZero() {
		daemon.ConfigModifiedAt = configMod.Format(time.RFC3339)
	}
	daemon.RestartRequired = daemon.PIDRunning && !daemonReference.IsZero() && configMod.After(daemonReference)

	sessions := settingsSessionsDTO{}
	if db, err := s.openStateDB(); err == nil {
		if totals, err := sessionTotals(db); err == nil {
			sessions.Total = totals.Total
			sessions.Running = totals.Running
			sessions.Ended = totals.Ended
		} else {
			sessions.Error = err.Error()
		}
		_ = db.Close()
	} else if !errors.Is(err, errStateDBUnset) {
		sessions.Error = err.Error()
	}

	mcp := settingsMCPDTO{
		Root:          mcpRoot,
		RootExists:    pathExists(mcpRoot),
		ConfigSurface: "catalog/mcp-servers/*.yaml",
	}
	if !mcpMod.IsZero() {
		mcp.ConfigModifiedAt = mcpMod.Format(time.RFC3339)
	}
	mcp.RestartRequired = daemon.PIDRunning && !daemonReference.IsZero() && mcpMod.After(daemonReference)
	if entries, err := config.LoadMCPServerCatalog(s.catalogRoot); err == nil {
		transports := map[string]struct{}{}
		mcp.Servers = len(entries)
		for _, entry := range entries {
			if entry.IsEnabled() {
				mcp.Enabled++
			}
			if entry.Token != "" {
				mcp.WithTokens++
			}
			if entry.Transport != "" {
				transports[entry.Transport] = struct{}{}
			}
		}
		for transport := range transports {
			mcp.Transports = append(mcp.Transports, transport)
		}
		sort.Strings(mcp.Transports)
	}

	providerRefs := providerLaunchRefs(cat)
	providers := make([]settingsProviderDTO, 0, len(cat.Providers))
	for _, provider := range cat.Providers {
		providers = append(providers, settingsProviderDTO{
			ID:                 provider.ID,
			Type:               provider.Type,
			Provider:           provider.ProviderBrand(),
			RuntimeKind:        provider.EffectiveRuntimeKind(),
			Command:            provider.Command,
			Args:               provider.Args,
			Adapter:            provider.Adapter,
			BootstrapMode:      provider.Bootstrap.Mode,
			BootstrapPrefix:    provider.Bootstrap.PromptPrefix,
			EnvMode:            provider.Env.Mode,
			EnvPassthrough:     provider.Env.Passthrough,
			EnvRedact:          provider.Env.Redact,
			ReferencedLaunches: providerRefs[provider.ID],
		})
	}
	sort.Slice(providers, func(i, j int) bool { return providers[i].ID < providers[j].ID })

	launches := settingsLaunchesDTO{Total: len(cat.Launches)}
	for _, launch := range cat.Launches {
		if len(launch.MCP.Servers) > 0 {
			launches.WithMCP++
		}
		if len(launch.Injection.NativeFiles)+len(launch.Injection.BootDirOverlay) > 0 {
			launches.WithInjection++
		}
		if len(launch.Overrides.Env) > 0 {
			launches.WithEnv++
		}
		if launch.Workspace.Mode == "worktree" || launch.Workspace.WorktreeName != "" {
			launches.WithWorktree++
		}
	}

	resp := settingsResponse{
		Server: s.settingsServer(),
		Paths: settingsPathsDTO{
			CatalogRoot:     pathDTO(s.catalogRoot),
			StateDB:         pathDTO(stateDB),
			WorkspaceRoot:   pathDTO(workspaceRoot),
			TempRoot:        pathDTO(tempRoot),
			LaunchSpecsRoot: pathDTO(launchSpecsRoot),
			ProjectsRoot:    pathDTO(catalogSubdir(s.catalogRoot, cat.Global.Catalog.Roots.Projects, "projects")),
			AgentsRoot:      pathDTO(catalogSubdir(s.catalogRoot, cat.Global.Catalog.Roots.Agents, "agents")),
			ProvidersRoot:   pathDTO(catalogSubdir(s.catalogRoot, cat.Global.Catalog.Roots.Providers, "providers")),
			LaunchesRoot:    pathDTO(catalogSubdir(s.catalogRoot, cat.Global.Catalog.Roots.Launches, "launches")),
			BootRoot:        pathDTO(catalogSubdir(s.catalogRoot, cat.Global.Catalog.Roots.Boot, "boot")),
			MCPServersRoot:  pathDTO(mcpRoot),
		},
		Daemon: daemon,
		Catalog: settingsCatalogDTO{
			Version:   cat.Global.Version,
			Projects:  len(cat.Projects),
			Agents:    len(cat.Agents),
			Providers: len(cat.Providers),
			Launches:  len(cat.Launches),
		},
		MCP:       mcp,
		Providers: providers,
		Launches:  launches,
		Sessions:  sessions,
		Roadmap: []settingsRoadmapDTO{
			{Area: "Setup", Status: "now", Next: "Dedicated settings tabs are in place; next harden with backups, diff previews, and stronger validation."},
			{Area: "MCP", Status: "now", Next: "Catalog CRUD, secret/env editing, and reload prompts are live; backend proxy modes and launch/project allowlists exist, so next expose effective proxy composition, lifecycle, and tool-scope policy."},
			{Area: "Tools", Status: "next", Next: "Replace usage-only rows with a live daemon tool registry, show native vs proxied vs discover-only visibility, and explain discovery scoring before adding ranking knobs."},
			{Area: "Broker", Status: "next", Next: "Backend envelopes already support priority, workflow/correlation IDs, replies, and wait-for-response; next add broker inspection and tracing because Sysop still only exposes messaging-store views."},
			{Area: "Registry", Status: "now", Next: "Agent/project browse, CRUD, sync, and bootstrap are live; next add callback editing support, richer filters, and group-kind admin."},
			{Area: "Proxy/Server", Status: "next", Next: "System health, socket/PID status, and guarded global edits are live; next add proxy readiness, listen/PID edits, and backup/diff support."},
			{Area: "Providers", Status: "now", Next: "Create/edit/delete with launch-reference guards is live; next add backups, diff previews, binary health checks, and capability guidance."},
			{Area: "Launches", Status: "now", Next: "Launch CRUD, worktree naming, prompt flags, and MCP selection are live; next add env/injection editors, checkbox MCP picker, and dry-run diff preview."},
			{Area: "Sessions", Status: "now", Next: "Wait/resize/live output/raw input/cleanup are live; next clarify create-launch-run-terminal semantics, align stop affordances to real runtime state, and make resume explicitly checkpoint-based new-session creation."},
		},
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *appServer) handleGlobalSettingsSave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, actionResponse{Error: "method not allowed"})
		return
	}
	var req globalSettingsSaveRequest
	if err := decodeJSONBody(r, &req, false); err != nil {
		writeJSON(w, http.StatusBadRequest, actionResponse{Error: "invalid request body: " + err.Error()})
		return
	}
	if req.ShutdownTimeout != "" {
		if _, err := time.ParseDuration(req.ShutdownTimeout); err != nil {
			writeJSON(w, http.StatusBadRequest, actionResponse{Error: "shutdown_timeout must be a Go duration, e.g. 10s"})
			return
		}
	}
	if !config.ValidPermissionMode(req.PermissionMode) {
		writeJSON(w, http.StatusBadRequest, actionResponse{Error: "permission_mode must be default or bypass"})
		return
	}
	switch req.LaunchEngine {
	case "", "catalog", "spec":
	default:
		writeJSON(w, http.StatusBadRequest, actionResponse{Error: "launch_engine must be catalog or spec"})
		return
	}

	globalPath := filepath.Join(s.catalogRoot, "global.yaml")
	var global config.Global
	if err := readYAMLFile(globalPath, &global); err != nil {
		writeJSON(w, http.StatusInternalServerError, actionResponse{Error: err.Error()})
		return
	}
	global.Daemon.ShutdownTimeout = strings.TrimSpace(req.ShutdownTimeout)
	global.Catalog.Defaults.PermissionMode = strings.TrimSpace(req.PermissionMode)
	global.Catalog.Defaults.LaunchEngine = strings.TrimSpace(req.LaunchEngine)
	global.Catalog.Defaults.LaunchSpecsRoot = strings.TrimSpace(req.LaunchSpecsRoot)
	global.Catalog.Defaults.WorkspaceRoot = strings.TrimSpace(req.WorkspaceRoot)
	global.Catalog.Defaults.StateDB = strings.TrimSpace(req.StateDB)
	global.Catalog.Defaults.TempRoot = strings.TrimSpace(req.TempRoot)

	backupPath, err := writeCatalogYAMLFile(globalPath, global, 0o600)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, actionResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, actionResponse{Status: "saved", BackupPath: backupPath})
}

func (s *appServer) handleProviderSave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, actionResponse{Error: "method not allowed"})
		return
	}
	var req providerSaveRequest
	if err := decodeJSONBody(r, &req, false); err != nil {
		writeJSON(w, http.StatusBadRequest, actionResponse{Error: "invalid request body: " + err.Error()})
		return
	}
	provider, path, err := s.providerFromSaveRequest(req)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, actionResponse{Error: err.Error()})
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		writeJSON(w, http.StatusInternalServerError, actionResponse{Error: err.Error()})
		return
	}
	backupPath, err := writeCatalogYAMLFile(path, provider, 0o644)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, actionResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, actionResponse{Status: "saved", BackupPath: backupPath})
}

func (s *appServer) handleProviderDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, actionResponse{Error: "method not allowed"})
		return
	}
	var req providerIDRequest
	if err := decodeJSONBody(r, &req, false); err != nil {
		writeJSON(w, http.StatusBadRequest, actionResponse{Error: "invalid request body: " + err.Error()})
		return
	}
	id := strings.TrimSpace(req.ID)
	if !validCatalogID(id) {
		writeJSON(w, http.StatusBadRequest, actionResponse{Error: "invalid id"})
		return
	}
	cat, err := config.Load(s.catalogRoot)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, actionResponse{Error: err.Error()})
		return
	}
	if refs := providerLaunchRefs(cat)[id]; refs > 0 {
		writeJSON(w, http.StatusConflict, actionResponse{Error: fmt.Sprintf("provider is used by %d launch profile(s)", refs)})
		return
	}
	_, path, found, err := s.rawProvider(id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, actionResponse{Error: err.Error()})
		return
	}
	if !found {
		writeJSON(w, http.StatusNotFound, actionResponse{Error: "provider not found"})
		return
	}
	backupPath, err := removeCatalogFileWithBackup(path)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, actionResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, actionResponse{Status: "deleted", BackupPath: backupPath})
}

func (s *appServer) handleAISettings(w http.ResponseWriter, _ *http.Request) {
	cat, err := config.Load(s.catalogRoot)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, aiSettingsResponse{Error: err.Error()})
		return
	}
	resp := aiSettingsResponse{Config: aiConfigFromConfig(cat.Global.AI)}
	if client, err := s.daemonClient(); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := client.Ping(ctx); err == nil {
			resp.Runtime.DaemonReachable = true
			if out, err := client.AIProviders(ctx); err == nil {
				resp.Runtime.Providers = len(out.Providers)
			} else if !isDaemonRouteMissing(err) {
				resp.Runtime.LastError = err.Error()
			}
			if out, err := client.AIModels(ctx, ""); err == nil {
				resp.Runtime.Models = len(out.Models)
			} else if !isDaemonRouteMissing(err) && resp.Runtime.LastError == "" {
				resp.Runtime.LastError = err.Error()
			}
			if out, err := client.AIRoutes(ctx); err == nil {
				resp.Runtime.Routes = len(out.Routes)
			} else if !isDaemonRouteMissing(err) && resp.Runtime.LastError == "" {
				resp.Runtime.LastError = err.Error()
			}
		} else {
			resp.Runtime.LastError = err.Error()
		}
	} else {
		resp.Runtime.LastError = err.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *appServer) handleAISettingsSave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, actionResponse{Error: "method not allowed"})
		return
	}
	var req aiSettingsSaveRequest
	if err := decodeJSONBody(r, &req, false); err != nil {
		writeJSON(w, http.StatusBadRequest, actionResponse{Error: "invalid request body: " + err.Error()})
		return
	}
	cat, err := config.Load(s.catalogRoot)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, actionResponse{Error: err.Error()})
		return
	}
	nextAI, err := configAIFromDTO(req.Config)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, actionResponse{Error: err.Error()})
		return
	}
	check := *cat
	check.Global = cat.Global
	check.Global.AI = nextAI
	if err := check.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, actionResponse{Error: err.Error()})
		return
	}
	globalPath := filepath.Join(s.catalogRoot, "global.yaml")
	global := cat.Global
	global.AI = nextAI
	backupPath, err := writeCatalogYAMLFile(globalPath, global, 0o600)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, actionResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, actionResponse{Status: "saved", BackupPath: backupPath})
}

func (s *appServer) handleAICatalogModels(w http.ResponseWriter, r *http.Request) {
	providerType := strings.TrimSpace(r.URL.Query().Get("provider_type"))
	vendorProviderID, ok := aiCatalogVendorProviderID(providerType)
	if !ok {
		writeJSON(w, http.StatusBadRequest, aiCatalogModelsResponse{Error: "unsupported provider_type"})
		return
	}

	catalog := modelcatalog.New()
	refs := catalog.List()
	fromCacheOnly := true
	if len(refs) == 0 {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if err := catalog.Refresh(ctx); err != nil {
			writeJSON(w, http.StatusBadGateway, aiCatalogModelsResponse{Error: err.Error()})
			return
		}
		refs = catalog.List()
		fromCacheOnly = false
	}

	resp := aiCatalogModelsResponse{
		ProviderType:     providerType,
		VendorProviderID: vendorProviderID,
		FromCacheOnly:    fromCacheOnly,
		Models:           make([]aiCatalogModelDTO, 0),
	}
	if ts := catalog.LastFetchedAt(); !ts.IsZero() {
		resp.LastFetchedAt = ts.UTC().Format(time.RFC3339)
	}
	for _, provider := range catalog.ListProviders() {
		if provider.ID == vendorProviderID {
			resp.VendorProvider = provider.Name
			break
		}
	}
	for _, ref := range refs {
		if ref.ProviderID != vendorProviderID {
			continue
		}
		resp.Models = append(resp.Models, aiCatalogModelFromRef(ref))
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *appServer) handleAIRuntime(w http.ResponseWriter, _ *http.Request) {
	resp := aiRuntimeResponse{}
	client, err := s.daemonClient()
	if err != nil {
		resp.Error = err.Error()
		writeJSON(w, http.StatusOK, resp)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	if out, err := client.AIProviders(ctx); err == nil {
		resp.Providers = out.Providers
	} else if isDaemonRouteMissing(err) {
		writeJSON(w, http.StatusOK, resp)
		return
	} else {
		resp.Error = err.Error()
		writeJSON(w, http.StatusOK, resp)
		return
	}
	if out, err := client.AIModels(ctx, ""); err == nil {
		resp.Models = out.Models
	} else if !isDaemonRouteMissing(err) && resp.Error == "" {
		resp.Error = err.Error()
	}
	if out, err := client.AIRoutes(ctx); err == nil {
		resp.Routes = out.Routes
	} else if !isDaemonRouteMissing(err) && resp.Error == "" {
		resp.Error = err.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *appServer) handleAIUsage(w http.ResponseWriter, r *http.Request) {
	resp := aiUsageResponse{}
	client, err := s.daemonClient()
	if err != nil {
		resp.Error = err.Error()
		writeJSON(w, http.StatusOK, resp)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
	defer cancel()
	out, err := client.AIUsage(ctx, muxclient.AIUsageQuery{
		Provider:  strings.TrimSpace(r.URL.Query().Get("provider")),
		Model:     strings.TrimSpace(r.URL.Query().Get("model")),
		SessionID: strings.TrimSpace(r.URL.Query().Get("session_id")),
		CallerID:  strings.TrimSpace(r.URL.Query().Get("caller_id")),
		Operation: strings.TrimSpace(r.URL.Query().Get("operation")),
		Since:     strings.TrimSpace(r.URL.Query().Get("since")),
	})
	if err != nil {
		if isDaemonRouteMissing(err) {
			writeJSON(w, http.StatusOK, resp)
			return
		}
		resp.Error = err.Error()
		writeJSON(w, http.StatusOK, resp)
		return
	}
	resp.Summary = out
	writeJSON(w, http.StatusOK, resp)
}

func (s *appServer) handleAIAudit(w http.ResponseWriter, r *http.Request) {
	resp := aiAuditResponse{}
	client, err := s.daemonClient()
	if err != nil {
		resp.Error = err.Error()
		writeJSON(w, http.StatusOK, resp)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
	defer cancel()
	limit, _ := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("limit")))
	out, err := client.AIAudit(ctx, muxclient.AIAuditQuery{
		EventType:  strings.TrimSpace(r.URL.Query().Get("event_type")),
		Provider:   strings.TrimSpace(r.URL.Query().Get("provider")),
		Model:      strings.TrimSpace(r.URL.Query().Get("model")),
		SessionID:  strings.TrimSpace(r.URL.Query().Get("session_id")),
		CallerID:   strings.TrimSpace(r.URL.Query().Get("caller_id")),
		Limit:      limit,
		Since:      strings.TrimSpace(r.URL.Query().Get("since")),
		ErrorsOnly: strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("errors_only")), "true"),
	})
	if err != nil {
		if isDaemonRouteMissing(err) {
			writeJSON(w, http.StatusOK, resp)
			return
		}
		resp.Error = err.Error()
		writeJSON(w, http.StatusOK, resp)
		return
	}
	resp.Events = out.Events
	resp.Count = out.Count
	writeJSON(w, http.StatusOK, resp)
}

func (s *appServer) handleAIBudgets(w http.ResponseWriter, r *http.Request) {
	resp := aiBudgetsResponse{}
	client, err := s.daemonClient()
	if err != nil {
		resp.Error = err.Error()
		writeJSON(w, http.StatusOK, resp)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
	defer cancel()
	out, err := client.AIBudgets(ctx, muxclient.AIBudgetsQuery{
		Provider:  strings.TrimSpace(r.URL.Query().Get("provider")),
		Model:     strings.TrimSpace(r.URL.Query().Get("model")),
		SessionID: strings.TrimSpace(r.URL.Query().Get("session_id")),
		CallerID:  strings.TrimSpace(r.URL.Query().Get("caller_id")),
	})
	if err != nil {
		if isDaemonRouteMissing(err) {
			writeJSON(w, http.StatusOK, resp)
			return
		}
		resp.Error = err.Error()
		writeJSON(w, http.StatusOK, resp)
		return
	}
	resp.Budgets = out.Budgets
	resp.Count = out.Count
	writeJSON(w, http.StatusOK, resp)
}

func isDaemonRouteMissing(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "daemon 404")
}

func (s *appServer) handleSystemResourceAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, actionResponse{Error: "method not allowed"})
		return
	}
	var req systemResourceActionRequest
	if err := decodeJSONBody(r, &req, false); err != nil {
		writeJSON(w, http.StatusBadRequest, actionResponse{Error: "invalid request body: " + err.Error()})
		return
	}
	if !allowedSystemResource(req.Resource) {
		writeJSON(w, http.StatusBadRequest, actionResponse{Error: "unsupported resource"})
		return
	}
	if !allowedSystemResourceAction(req.Action) {
		writeJSON(w, http.StatusBadRequest, actionResponse{Error: "unsupported action"})
		return
	}
	if req.Resource == "tether-dev" && req.Action != "status" {
		go func(resource, action string) {
			time.Sleep(300 * time.Millisecond)
			_, _ = runCerberusResourceAction(context.Background(), resource, action)
		}(req.Resource, req.Action)
		writeJSON(w, http.StatusAccepted, actionResponse{Status: "scheduled"})
		return
	}
	output, err := runCerberusResourceAction(r.Context(), req.Resource, req.Action)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, actionResponse{Error: err.Error(), Output: output})
		return
	}
	writeJSON(w, http.StatusOK, actionResponse{Status: "ok", Output: output})
}

func (s *appServer) handleCatalog(w http.ResponseWriter, _ *http.Request) {
	cat, err := config.Load(s.catalogRoot)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	resp := catalogResponse{
		Projects:  make([]projectDTO, 0, len(cat.Projects)),
		Agents:    make([]agentDTO, 0, len(cat.Agents)),
		Providers: make([]providerDTO, 0, len(cat.Providers)),
		Launches:  make([]launchDTO, 0, len(cat.Launches)),
	}
	for _, p := range cat.Projects {
		resp.Projects = append(resp.Projects, projectDTO{
			ID:         p.ID,
			Name:       p.Name,
			RepoRoot:   config.Expand(p.RepoRoot),
			Mode:       p.Workspace.DefaultMode,
			MCPServers: append([]string(nil), p.MCP.Servers...),
		})
	}
	for _, a := range cat.Agents {
		resp.Agents = append(resp.Agents, agentDTO{
			ID:     a.ID,
			Name:   a.Name,
			Roles:  a.Roles,
			Skills: a.Skills,
		})
	}
	for _, p := range cat.Providers {
		resp.Providers = append(resp.Providers, providerDTO{
			ID:          p.ID,
			Type:        p.Type,
			Provider:    p.Provider,
			RuntimeKind: p.RuntimeKind,
			Command:     p.Command,
		})
	}
	for _, l := range cat.Launches {
		profileJSON := prettyJSON(l)
		planJSON := ""
		planError := ""
		if plan, err := launchplan.Resolve(cat, launchplan.Input{
			LaunchID:    l.ID,
			CatalogRoot: s.catalogRoot,
		}); err != nil {
			planError = err.Error()
		} else {
			planJSON = prettyJSON(plan)
		}
		resp.Launches = append(resp.Launches, launchDTO{
			ID:            l.ID,
			Project:       l.Project,
			Agent:         l.Agent,
			Provider:      l.Provider,
			WorkspaceMode: l.Workspace.Mode,
			NativeFiles:   len(l.Injection.NativeFiles),
			BootOverlay:   len(l.Injection.BootDirOverlay),
			Profile:       profileJSON,
			LaunchPlan:    planJSON,
			PlanError:     planError,
		})
	}

	sort.Slice(resp.Projects, func(i, j int) bool { return resp.Projects[i].ID < resp.Projects[j].ID })
	sort.Slice(resp.Agents, func(i, j int) bool { return resp.Agents[i].ID < resp.Agents[j].ID })
	sort.Slice(resp.Providers, func(i, j int) bool { return resp.Providers[i].ID < resp.Providers[j].ID })
	sort.Slice(resp.Launches, func(i, j int) bool { return resp.Launches[i].ID < resp.Launches[j].ID })

	writeJSON(w, http.StatusOK, resp)
}

func (s *appServer) handleLaunchProfile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, actionResponse{Error: "method not allowed"})
		return
	}
	var req launchActionRequest
	if err := decodeJSONBody(r, &req, false); err != nil {
		writeJSON(w, http.StatusBadRequest, actionResponse{Error: "invalid request body: " + err.Error()})
		return
	}
	if strings.TrimSpace(req.LaunchID) == "" {
		writeJSON(w, http.StatusBadRequest, actionResponse{Error: "launch_id required"})
		return
	}
	daemonClient, err := s.daemonClient()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, actionResponse{Error: err.Error()})
		return
	}
	res, err := daemonClient.Launch(r.Context(), req.LaunchID)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, muxclient.ErrDaemonUnreachable) {
			status = http.StatusServiceUnavailable
		}
		writeJSON(w, status, actionResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, actionResponse{
		Status:         "launched",
		SessionID:      res.ID,
		Workspace:      res.Workspace,
		Log:            res.Log,
		ProviderID:     res.ProviderID,
		ProviderKind:   res.ProviderKind,
		LogicalAgentID: res.LogicalAgentID,
	})
}

func (s *appServer) handleLaunchSave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, actionResponse{Error: "method not allowed"})
		return
	}
	var req launchSaveRequest
	if err := decodeJSONBody(r, &req, false); err != nil {
		writeJSON(w, http.StatusBadRequest, actionResponse{Error: "invalid request body: " + err.Error()})
		return
	}
	change, err := s.previewLaunchChange(req)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, actionResponse{Error: err.Error()})
		return
	}
	if err := os.MkdirAll(filepath.Dir(change.Path), 0o755); err != nil {
		writeJSON(w, http.StatusInternalServerError, actionResponse{Error: err.Error()})
		return
	}
	backupPath, err := writeCatalogYAMLFile(change.Path, change.Launch, 0o644)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, actionResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, actionResponse{Status: "saved", BackupPath: backupPath})
}

func (s *appServer) handleLaunchPreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, launchPreviewResponse{Error: "method not allowed"})
		return
	}
	var req launchSaveRequest
	if err := decodeJSONBody(r, &req, false); err != nil {
		writeJSON(w, http.StatusBadRequest, launchPreviewResponse{Error: "invalid request body: " + err.Error()})
		return
	}
	change, err := s.previewLaunchChange(req)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, launchPreviewResponse{Error: err.Error()})
		return
	}
	resp := launchPreviewResponse{
		Existing:           change.Found,
		CurrentProfile:     change.CurrentProfile,
		NextProfile:        change.NextProfile,
		CurrentPlan:        change.CurrentPlan,
		NextPlan:           change.NextPlan,
		CurrentProfileYAML: change.CurrentProfileYAML,
		NextProfileYAML:    change.NextProfileYAML,
		ProfileDiff:        change.ProfileDiff,
		PlanDiff:           change.PlanDiff,
		ProfileChanged:     change.ProfileChanged,
		PlanChanged:        change.PlanChanged,
		TargetPath:         change.Path,
		WillCreateBackup:   change.WillCreateBackup,
		CommentLossRisk:    change.CommentLossRisk,
		CurrentPlanError:   change.CurrentPlanError,
		NextPlanError:      change.NextPlanError,
		Warnings:           change.Warnings,
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *appServer) handleLaunchDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, actionResponse{Error: "method not allowed"})
		return
	}
	var req launchActionRequest
	if err := decodeJSONBody(r, &req, false); err != nil {
		writeJSON(w, http.StatusBadRequest, actionResponse{Error: "invalid request body: " + err.Error()})
		return
	}
	id := strings.TrimSpace(req.LaunchID)
	if !validCatalogID(id) {
		writeJSON(w, http.StatusBadRequest, actionResponse{Error: "invalid launch_id"})
		return
	}
	_, path, found, err := s.rawLaunch(id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, actionResponse{Error: err.Error()})
		return
	}
	if !found {
		writeJSON(w, http.StatusNotFound, actionResponse{Error: "launch not found"})
		return
	}
	backupPath, err := removeCatalogFileWithBackup(path)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, actionResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, actionResponse{Status: "deleted", BackupPath: backupPath})
}

func (s *appServer) handleSessionStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, actionResponse{Error: "method not allowed"})
		return
	}
	var req sessionActionRequest
	if err := decodeJSONBody(r, &req, false); err != nil {
		writeJSON(w, http.StatusBadRequest, actionResponse{Error: "invalid request body: " + err.Error()})
		return
	}
	if strings.TrimSpace(req.ID) == "" {
		writeJSON(w, http.StatusBadRequest, actionResponse{Error: "id required"})
		return
	}
	daemonClient, err := s.daemonClient()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, actionResponse{Error: err.Error()})
		return
	}
	if err := daemonClient.StopSession(r.Context(), req.ID); err != nil {
		writeDaemonActionError(w, err)
		return
	}
	s.recordOperatorEvent(events.ScopeSession, req.ID, kindSysopSessionStop, map[string]any{
		"source": "sysop",
		"action": "stop",
	})
	writeJSON(w, http.StatusOK, actionResponse{Status: "stopped", SessionID: req.ID})
}

func (s *appServer) handleSessionTurn(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, actionResponse{Error: "method not allowed"})
		return
	}
	var req sessionTextActionRequest
	if err := decodeJSONBody(r, &req, false); err != nil {
		writeJSON(w, http.StatusBadRequest, actionResponse{Error: "invalid request body: " + err.Error()})
		return
	}
	if strings.TrimSpace(req.ID) == "" || strings.TrimSpace(req.Text) == "" {
		writeJSON(w, http.StatusBadRequest, actionResponse{Error: "id and text required"})
		return
	}
	daemonClient, err := s.daemonClient()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, actionResponse{Error: err.Error()})
		return
	}
	if err := daemonClient.SendTurn(r.Context(), req.ID, req.Text); err != nil {
		writeDaemonActionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, actionResponse{Status: "sent", SessionID: req.ID})
}

func (s *appServer) handleSessionInput(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, actionResponse{Error: "method not allowed"})
		return
	}
	var req sessionTextActionRequest
	if err := decodeJSONBody(r, &req, false); err != nil {
		writeJSON(w, http.StatusBadRequest, actionResponse{Error: "invalid request body: " + err.Error()})
		return
	}
	if strings.TrimSpace(req.ID) == "" || req.Text == "" {
		writeJSON(w, http.StatusBadRequest, actionResponse{Error: "id and text required"})
		return
	}
	daemonClient, err := s.daemonClient()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, actionResponse{Error: err.Error()})
		return
	}
	if err := daemonClient.SendInput(r.Context(), req.ID, []byte(req.Text)); err != nil {
		writeDaemonActionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, actionResponse{Status: "sent", SessionID: req.ID})
}

func (s *appServer) handleSessionResize(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, actionResponse{Error: "method not allowed"})
		return
	}
	var req sessionResizeActionRequest
	if err := decodeJSONBody(r, &req, false); err != nil {
		writeJSON(w, http.StatusBadRequest, actionResponse{Error: "invalid request body: " + err.Error()})
		return
	}
	if strings.TrimSpace(req.ID) == "" || req.Rows == 0 || req.Cols == 0 {
		writeJSON(w, http.StatusBadRequest, actionResponse{Error: "id, rows, and cols required"})
		return
	}
	daemonClient, err := s.daemonClient()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, actionResponse{Error: err.Error()})
		return
	}
	if err := daemonClient.ResizeSession(r.Context(), req.ID, req.Rows, req.Cols); err != nil {
		writeDaemonActionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, actionResponse{Status: "resized", SessionID: req.ID})
}

func (s *appServer) handleSessionWait(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, actionResponse{Error: "method not allowed"})
		return
	}
	var req sessionActionRequest
	if err := decodeJSONBody(r, &req, false); err != nil {
		writeJSON(w, http.StatusBadRequest, actionResponse{Error: "invalid request body: " + err.Error()})
		return
	}
	if strings.TrimSpace(req.ID) == "" {
		writeJSON(w, http.StatusBadRequest, actionResponse{Error: "id required"})
		return
	}
	daemonClient, err := s.daemonClient()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, actionResponse{Error: err.Error()})
		return
	}
	exitCode, err := daemonClient.WaitSession(r.Context(), req.ID)
	if err != nil {
		writeDaemonActionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, actionResponse{Status: "exited", SessionID: req.ID, ExitCode: &exitCode})
}

func (s *appServer) handleSessionAttach(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, actionResponse{Error: "method not allowed"})
		return
	}
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		writeJSON(w, http.StatusBadRequest, actionResponse{Error: "id query param required"})
		return
	}
	var sinceSeq int64
	if raw := strings.TrimSpace(r.URL.Query().Get("since_seq")); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 0 {
			writeJSON(w, http.StatusBadRequest, actionResponse{Error: "since_seq must be a non-negative integer"})
			return
		}
		sinceSeq = n
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, actionResponse{Error: "streaming not supported"})
		return
	}
	daemonClient, err := s.daemonClient()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, actionResponse{Error: err.Error()})
		return
	}
	session, err := daemonClient.GetSession(r.Context(), id)
	if err != nil {
		writeDaemonActionError(w, err)
		return
	}
	if session.State != "running" {
		writeJSON(w, http.StatusConflict, actionResponse{Error: "session is not running"})
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	if err := daemonClient.AttachSession(r.Context(), id, flushResponseWriter{w: w, f: flusher}, sinceSeq); err != nil {
		return
	}
}

func (s *appServer) handleSessionCleanup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, actionResponse{Error: "method not allowed"})
		return
	}
	var req sessionCleanupRequest
	if err := decodeJSONBody(r, &req, false); err != nil {
		writeJSON(w, http.StatusBadRequest, actionResponse{Error: "invalid request body: " + err.Error()})
		return
	}
	if req.OlderThanDays < 1 {
		writeJSON(w, http.StatusBadRequest, actionResponse{Error: "older_than_days must be >= 1"})
		return
	}
	if req.Limit < 1 {
		req.Limit = 100
	}
	if req.Limit > 1000 {
		req.Limit = 1000
	}
	db, err := s.openStateDB()
	if errors.Is(err, errStateDBUnset) {
		writeJSON(w, http.StatusServiceUnavailable, actionResponse{Error: "no state db configured"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, actionResponse{Error: err.Error()})
		return
	}
	defer db.Close()
	cutoff := time.Now().UTC().AddDate(0, 0, -req.OlderThanDays).Format(time.RFC3339)
	ids, err := cleanupCandidateSessionIDs(db.DB(), cutoff, req.Limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, actionResponse{Error: err.Error()})
		return
	}
	if req.DryRun || len(ids) == 0 {
		writeJSON(w, http.StatusOK, actionResponse{Status: "preview", Count: len(ids)})
		return
	}
	if err := deleteSessionsByID(db.DB(), ids); err != nil {
		writeJSON(w, http.StatusInternalServerError, actionResponse{Error: err.Error()})
		return
	}
	s.recordOperatorEvent(events.ScopeDaemon, "", kindSysopSessionCleanup, map[string]any{
		"source":          "sysop",
		"action":          "cleanup",
		"older_than_days": req.OlderThanDays,
		"limit":           req.Limit,
		"deleted_count":   len(ids),
		"session_ids":     ids,
	})
	writeJSON(w, http.StatusOK, actionResponse{Status: "deleted", Count: len(ids)})
}

func (s *appServer) handleSessionCheckpoint(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, actionResponse{Error: "method not allowed"})
		return
	}
	var req checkpointActionRequest
	if err := decodeJSONBody(r, &req, false); err != nil {
		writeJSON(w, http.StatusBadRequest, actionResponse{Error: "invalid request body: " + err.Error()})
		return
	}
	if strings.TrimSpace(req.ID) == "" {
		writeJSON(w, http.StatusBadRequest, actionResponse{Error: "id required"})
		return
	}
	daemonClient, err := s.daemonClient()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, actionResponse{Error: err.Error()})
		return
	}
	if err := daemonClient.CreateCheckpoint(r.Context(), req.ID, req.Status, req.Summary); err != nil {
		writeDaemonActionError(w, err)
		return
	}
	s.recordOperatorEvent(events.ScopeSession, req.ID, kindSysopSessionCheckpoint, map[string]any{
		"source":  "sysop",
		"action":  "checkpoint",
		"status":  strings.TrimSpace(req.Status),
		"summary": strings.TrimSpace(req.Summary),
	})
	writeJSON(w, http.StatusOK, actionResponse{Status: "checkpointed", SessionID: req.ID})
}

func (s *appServer) handleLogicalAgentResume(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, actionResponse{Error: "method not allowed"})
		return
	}
	var req resumeActionRequest
	if err := decodeJSONBody(r, &req, false); err != nil {
		writeJSON(w, http.StatusBadRequest, actionResponse{Error: "invalid request body: " + err.Error()})
		return
	}
	if strings.TrimSpace(req.LogicalAgentID) == "" {
		writeJSON(w, http.StatusBadRequest, actionResponse{Error: "logical_agent_id required"})
		return
	}
	daemonClient, err := s.daemonClient()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, actionResponse{Error: err.Error()})
		return
	}
	res, err := daemonClient.ResumeLogicalAgent(r.Context(), req.LogicalAgentID)
	if err != nil {
		writeDaemonActionError(w, err)
		return
	}
	s.recordOperatorEvent(events.ScopeSession, res.ID, kindSysopLogicalResume, map[string]any{
		"source":           "sysop",
		"action":           "resume",
		"logical_agent_id": req.LogicalAgentID,
		"provider_id":      res.ProviderID,
		"provider_kind":    res.ProviderKind,
	})
	writeJSON(w, http.StatusCreated, actionResponse{
		Status:         "resumed",
		SessionID:      res.ID,
		Workspace:      res.Workspace,
		Log:            res.Log,
		ProviderID:     res.ProviderID,
		ProviderKind:   res.ProviderKind,
		LogicalAgentID: res.LogicalAgentID,
	})
}

func (s *appServer) handleLogicalAgentPolicy(w http.ResponseWriter, r *http.Request) {
	daemonClient, err := s.daemonClient()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, logicalAgentPolicyResponse{Error: err.Error()})
		return
	}
	switch r.Method {
	case http.MethodGet:
		id := strings.TrimSpace(r.URL.Query().Get("id"))
		if id == "" {
			writeJSON(w, http.StatusBadRequest, logicalAgentPolicyResponse{Error: "id required"})
			return
		}
		res, err := daemonClient.GetLogicalAgentPolicy(r.Context(), id)
		if err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, muxclient.ErrDaemonUnreachable) {
				status = http.StatusServiceUnavailable
			}
			writeJSON(w, status, logicalAgentPolicyResponse{Error: err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, logicalAgentPolicyResponse{
			LogicalAgentID:   res.LogicalAgentID,
			Name:             res.Name,
			LaunchID:         res.LaunchID,
			CheckpointPolicy: res.CheckpointPolicy,
			CheckpointStatus: res.CheckpointStatus,
			UpdatedAt:        res.UpdatedAt,
		})
	case http.MethodPost:
		var req logicalAgentPolicySaveRequest
		if err := decodeJSONBody(r, &req, false); err != nil {
			writeJSON(w, http.StatusBadRequest, logicalAgentPolicyResponse{Error: "invalid request body: " + err.Error()})
			return
		}
		if strings.TrimSpace(req.LogicalAgentID) == "" {
			writeJSON(w, http.StatusBadRequest, logicalAgentPolicyResponse{Error: "logical_agent_id required"})
			return
		}
		res, err := daemonClient.UpdateLogicalAgentPolicy(r.Context(), agent.LogicalAgentPolicy{
			LogicalAgentID:   strings.TrimSpace(req.LogicalAgentID),
			CheckpointPolicy: agent.CheckpointPolicy(strings.TrimSpace(req.CheckpointPolicy)),
			CheckpointStatus: strings.TrimSpace(req.CheckpointStatus),
		})
		if err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, muxclient.ErrDaemonUnreachable) {
				status = http.StatusServiceUnavailable
			}
			writeJSON(w, status, logicalAgentPolicyResponse{Error: err.Error()})
			return
		}
		s.recordOperatorEvent(events.ScopeDaemon, "", kindSysopLogicalAgentPolicy, map[string]any{
			"source":            "sysop",
			"action":            "logical_agent_policy_update",
			"logical_agent_id":  res.LogicalAgentID,
			"checkpoint_policy": res.CheckpointPolicy,
			"checkpoint_status": res.CheckpointStatus,
		})
		writeJSON(w, http.StatusOK, logicalAgentPolicyResponse{
			LogicalAgentID:   res.LogicalAgentID,
			Name:             res.Name,
			LaunchID:         res.LaunchID,
			CheckpointPolicy: res.CheckpointPolicy,
			CheckpointStatus: res.CheckpointStatus,
			UpdatedAt:        res.UpdatedAt,
		})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, logicalAgentPolicyResponse{Error: "method not allowed"})
	}
}

func (s *appServer) handleSessions(w http.ResponseWriter, _ *http.Request) {
	cat, err := config.Load(s.catalogRoot)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, sessionsResponse{Error: err.Error()})
		return
	}
	dbPath := config.Expand(cat.Global.Catalog.Defaults.StateDB)
	if dbPath == "" {
		writeJSON(w, http.StatusOK, sessionsResponse{Sessions: []sessionDTO{}})
		return
	}
	db, err := store.Open(dbPath)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, sessionsResponse{Error: err.Error()})
		return
	}
	defer db.Close()

	totals, err := sessionTotals(db)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, sessionsResponse{Error: err.Error()})
		return
	}
	rows, err := db.ListSessions(store.ListSessionsOptions{Limit: 50})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, sessionsResponse{Error: err.Error()})
		return
	}
	out := make([]sessionDTO, 0, len(rows))
	for _, row := range rows {
		out = append(out, sessionDTO{
			ID:             row.ID,
			LaunchID:       row.LaunchID,
			ProjectID:      row.ProjectID,
			LogicalAgentID: row.LogicalAgentID,
			ProviderID:     row.ProviderID,
			ProviderKind:   row.ProviderKind,
			Workspace:      row.Workspace,
			State:          row.State,
			PID:            nullableInt(row.PID.Valid, int(row.PID.Int64)),
			ExitCode:       nullableInt(row.ExitCode.Valid, int(row.ExitCode.Int64)),
			CreatedAt:      row.CreatedAt,
			UpdatedAt:      row.UpdatedAt,
			EndedAt:        nullableString(row.EndedAt.Valid, row.EndedAt.String),
		})
	}
	writeJSON(w, http.StatusOK, sessionsResponse{
		Sessions: out,
		Total:    totals.Total,
		Running:  totals.Running,
		Ended:    totals.Ended,
	})
}

type sessionStats struct {
	Total   int
	Running int
	Ended   int
}

func sessionTotals(db *store.Store) (sessionStats, error) {
	rows, err := db.DB().Query(`SELECT state, COALESCE(ended_at, '') FROM sessions`)
	if err != nil {
		return sessionStats{}, err
	}
	defer rows.Close()

	var stats sessionStats
	for rows.Next() {
		var state, endedAt string
		if err := rows.Scan(&state, &endedAt); err != nil {
			return sessionStats{}, err
		}
		stats.Total++
		if state == "running" {
			stats.Running++
		}
		if endedAt != "" {
			stats.Ended++
		}
	}
	return stats, rows.Err()
}

// ─── Messages ────────────────────────────────────────────────────────────────

type messagesResponse struct {
	Messages []messageDTO  `json:"messages"`
	Totals   messageTotals `json:"totals"`
	Error    string        `json:"error,omitempty"`
}

type messageTotals struct {
	Total  int               `json:"total"`
	User   messageScopeStats `json:"user"`
	Agent  messageScopeStats `json:"agent"`
	Other  messageScopeStats `json:"other"`
	Groups messageScopeStats `json:"groups"`
}

type messageScopeStats struct {
	Total    int `json:"total"`
	Unread   int `json:"unread"`
	Archived int `json:"archived"`
}

type messageDTO struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Channel   string `json:"channel,omitempty"`
	From      string `json:"from"`
	To        string `json:"to"`
	ThreadID  string `json:"thread_id,omitempty"`
	InReplyTo string `json:"in_reply_to,omitempty"`
	// Subject and Body are the projected display fields; Payload is the raw
	// (possibly structured-JSON) message payload, kept for detail views.
	Subject     string `json:"subject,omitempty"`
	Body        string `json:"body"`
	Payload     string `json:"payload,omitempty"`
	ContentType string `json:"content_type,omitempty"`
	// Scope is derived from the recipient URN kind: "user", "agent", or "other".
	Scope       string `json:"scope"`
	CreatedAt   string `json:"created_at"`
	DeliveredAt string `json:"delivered_at,omitempty"`
	ConsumedAt  string `json:"consumed_at,omitempty"`
	CanceledAt  string `json:"canceled_at,omitempty"`
	ReadAt      string `json:"read_at,omitempty"`
	ArchivedAt  string `json:"archived_at,omitempty"`
}

type replyRequest struct {
	From        string `json:"from"`
	To          string `json:"to"`
	Kind        string `json:"kind"`
	Body        string `json:"body"`
	InReplyTo   string `json:"in_reply_to"`
	ThreadID    string `json:"thread_id"`
	ContentType string `json:"content_type"`
}

type groupsResponse struct {
	Groups []groupDTO `json:"groups"`
	Error  string     `json:"error,omitempty"`
}

type groupDTO struct {
	URN         string            `json:"urn"`
	DisplayName string            `json:"display_name"`
	Title       string            `json:"title,omitempty"`
	Description string            `json:"description,omitempty"`
	Status      string            `json:"status"`
	CreatedAt   string            `json:"created_at"`
	UpdatedAt   string            `json:"updated_at"`
	Members     []groupMemberDTO  `json:"members"`
	Messages    []groupMessageDTO `json:"messages"`
	updatedAt   time.Time
}

type groupMemberDTO struct {
	MemberURN   string `json:"member_urn"`
	DisplayName string `json:"display_name,omitempty"`
	Role        string `json:"role"`
	JoinedAt    string `json:"joined_at"`
	LastReadSeq int64  `json:"last_read_seq"`
}

type groupMessageDTO struct {
	ID          string `json:"id"`
	GroupURN    string `json:"group_urn"`
	GroupSeq    int64  `json:"group_seq"`
	FromURN     string `json:"from_urn"`
	Kind        string `json:"kind"`
	ThreadID    string `json:"thread_id,omitempty"`
	Subject     string `json:"subject,omitempty"`
	Body        string `json:"body"`
	Payload     string `json:"payload,omitempty"`
	ContentType string `json:"content_type,omitempty"`
	CreatedAt   string `json:"created_at"`
}

type groupReplyRequest struct {
	GroupURN    string `json:"group_urn"`
	From        string `json:"from"`
	Kind        string `json:"kind"`
	Body        string `json:"body"`
	ThreadID    string `json:"thread_id,omitempty"`
	ContentType string `json:"content_type,omitempty"`
}

type groupCreateRequest struct {
	DisplayName string `json:"display_name"`
	Description string `json:"description,omitempty"`
	CreatorURN  string `json:"creator_urn"`
}

type messageAgentsResponse struct {
	Agents []messageAgentDTO `json:"agents"`
	Error  string            `json:"error,omitempty"`
}

type messageAgentDTO struct {
	URN         string `json:"urn"`
	DisplayName string `json:"display_name"`
	Title       string `json:"title,omitempty"`
	Status      string `json:"status"`
}

// handleMessages serves GET (non-destructive list) and POST (send/reply).
func (s *appServer) handleMessages(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleMessagesList(w, r)
	case http.MethodPost:
		s.handleMessageReply(w, r)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, messagesResponse{Error: "method not allowed"})
	}
}

func (s *appServer) handleMessageGroups(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleMessageGroupsList(w, r)
	case http.MethodPost:
		s.handleMessageGroupReply(w, r)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, groupsResponse{Error: "method not allowed"})
	}
}

func (s *appServer) handleMessageGroupCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, groupsResponse{Error: "method not allowed"})
		return
	}
	var req groupCreateRequest
	if err := decodeJSONBody(r, &req, false); err != nil {
		writeJSON(w, http.StatusBadRequest, groupsResponse{Error: "invalid body: " + err.Error()})
		return
	}
	if strings.TrimSpace(req.DisplayName) == "" || strings.TrimSpace(req.CreatorURN) == "" {
		writeJSON(w, http.StatusBadRequest, groupsResponse{Error: "display_name and creator_urn are required"})
		return
	}

	db, err := s.openStateDB()
	if errors.Is(err, errStateDBUnset) {
		writeJSON(w, http.StatusServiceUnavailable, groupsResponse{Error: "no state db configured"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, groupsResponse{Error: err.Error()})
		return
	}
	defer db.Close()

	regStore := registry.NewStorage(db.DB())
	svc := registry.NewService(regStore)
	group, err := svc.Register(r.Context(), registry.KindGroup, registry.Profile{
		DisplayName:   strings.TrimSpace(req.DisplayName),
		Description:   strings.TrimSpace(req.Description),
		LastUpdatedBy: strings.TrimSpace(req.CreatorURN),
	})
	if err != nil {
		writeRegistryActionError(w, err)
		return
	}
	members, err := regStore.ListMembers(r.Context(), group.URN)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, groupsResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, groupToDTO(group, members, nil))
}

func (s *appServer) handleMessageAgents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, messageAgentsResponse{Error: "method not allowed"})
		return
	}
	db, err := s.openStateDB()
	if errors.Is(err, errStateDBUnset) {
		writeJSON(w, http.StatusOK, messageAgentsResponse{Agents: []messageAgentDTO{}})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, messageAgentsResponse{Error: err.Error()})
		return
	}
	defer db.Close()

	regStore := registry.NewStorage(db.DB())
	agents, err := regStore.Search(r.Context(), registry.KindAgent, registry.Filter{Status: registry.StatusAny})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, messageAgentsResponse{Error: err.Error()})
		return
	}
	out := make([]messageAgentDTO, 0, len(agents))
	for _, a := range agents {
		out = append(out, messageAgentDTO{
			URN:         a.URN,
			DisplayName: a.DisplayName,
			Title:       a.Title,
			Status:      string(a.Status),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].DisplayName != out[j].DisplayName {
			return out[i].DisplayName < out[j].DisplayName
		}
		return out[i].URN < out[j].URN
	})
	writeJSON(w, http.StatusOK, messageAgentsResponse{Agents: out})
}

func (s *appServer) handleBrokerEnvelopes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, brokerEnvelopesResponse{Error: "method not allowed"})
		return
	}
	db, err := s.openStateDB()
	if errors.Is(err, errStateDBUnset) {
		writeJSON(w, http.StatusOK, brokerEnvelopesResponse{Envelopes: []brokerEnvelopeDTO{}})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, brokerEnvelopesResponse{Error: err.Error()})
		return
	}
	defer db.Close()

	workflowID := strings.TrimSpace(r.URL.Query().Get("workflow_id"))
	recipient := strings.TrimSpace(r.URL.Query().Get("recipient"))
	correlationID := strings.TrimSpace(r.URL.Query().Get("correlation_id"))
	order := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("order")))
	switch order {
	case "", "desc":
		order = "DESC"
	case "asc":
		order = "ASC"
	default:
		writeJSON(w, http.StatusBadRequest, brokerEnvelopesResponse{Error: "invalid order"})
		return
	}
	limit := 100
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			writeJSON(w, http.StatusBadRequest, brokerEnvelopesResponse{Error: "invalid limit"})
			return
		}
		if n > 500 {
			n = 500
		}
		limit = n
	}

	query := `SELECT id, COALESCE(sender, ''), COALESCE(recipient, ''), COALESCE(workflow_id, ''),
COALESCE(correlation_id, ''), COALESCE(message_type, ''), COALESCE(priority, 0), COALESCE(payload, ''),
COALESCE(created_at, ''), COALESCE(delivered_at, ''), COALESCE(consumed_at, ''), COALESCE(audit_json, '')
FROM broker_envelopes`
	var clauses []string
	var args []any
	if workflowID != "" {
		clauses = append(clauses, "workflow_id = ?")
		args = append(args, workflowID)
	}
	if recipient != "" {
		clauses = append(clauses, "recipient = ?")
		args = append(args, recipient)
	}
	if correlationID != "" {
		clauses = append(clauses, "correlation_id = ?")
		args = append(args, correlationID)
	}
	if len(clauses) > 0 {
		query += " WHERE " + strings.Join(clauses, " AND ")
	}
	query += " ORDER BY created_at " + order + fmt.Sprintf(" LIMIT %d", limit)

	rows, err := db.DB().Query(query, args...)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, brokerEnvelopesResponse{Error: err.Error()})
		return
	}
	defer rows.Close()

	var out []brokerEnvelopeDTO
	for rows.Next() {
		var env brokerEnvelopeDTO
		if err := rows.Scan(
			&env.ID,
			&env.Sender,
			&env.Recipient,
			&env.WorkflowID,
			&env.CorrelationID,
			&env.MessageType,
			&env.Priority,
			&env.Payload,
			&env.CreatedAt,
			&env.DeliveredAt,
			&env.ConsumedAt,
			&env.AuditJSON,
		); err != nil {
			writeJSON(w, http.StatusInternalServerError, brokerEnvelopesResponse{Error: err.Error()})
			return
		}
		out = append(out, env)
	}
	if err := rows.Err(); err != nil {
		writeJSON(w, http.StatusInternalServerError, brokerEnvelopesResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, brokerEnvelopesResponse{Envelopes: out})
}

func (s *appServer) handleMessagesList(w http.ResponseWriter, _ *http.Request) {
	db, err := s.openStateDB()
	if errors.Is(err, errStateDBUnset) {
		writeJSON(w, http.StatusOK, messagesResponse{Messages: []messageDTO{}})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, messagesResponse{Error: err.Error()})
		return
	}
	defer db.Close()

	totals, err := countMessages(db)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, messagesResponse{Error: err.Error()})
		return
	}
	rows, err := db.ListMessages(500)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, messagesResponse{Error: err.Error()})
		return
	}
	out := make([]messageDTO, 0, len(rows))
	for _, m := range rows {
		out = append(out, messageDTO{
			ID:          m.ID,
			Kind:        m.Kind,
			Channel:     m.Channel,
			From:        m.FromURN,
			To:          m.ToURN,
			ThreadID:    m.ThreadID,
			InReplyTo:   m.InReplyTo,
			Subject:     m.Subject,
			Body:        m.Body,
			Payload:     m.Payload,
			ContentType: m.ContentType,
			Scope:       scopeOf(m.ToURN),
			CreatedAt:   m.CreatedAt,
			DeliveredAt: m.DeliveredAt,
			ConsumedAt:  m.ConsumedAt,
			CanceledAt:  m.CanceledAt,
			ReadAt:      m.ReadAt,
			ArchivedAt:  m.ArchivedAt,
		})
	}
	writeJSON(w, http.StatusOK, messagesResponse{Messages: out, Totals: totals})
}

func countMessages(db *store.Store) (messageTotals, error) {
	rows, err := db.DB().Query(
		`SELECT to_urn, COALESCE(read_at, ''), COALESCE(archived_at, ''),
		        COALESCE(canceled_at, ''), COALESCE(group_urn, '')
		   FROM messages`)
	if err != nil {
		return messageTotals{}, err
	}
	defer rows.Close()

	var totals messageTotals
	for rows.Next() {
		var toURN, readAt, archivedAt, canceledAt, groupURN string
		if err := rows.Scan(&toURN, &readAt, &archivedAt, &canceledAt, &groupURN); err != nil {
			return messageTotals{}, err
		}
		totals.Total++
		var bucket *messageScopeStats
		if groupURN != "" {
			bucket = &totals.Groups
		} else {
			switch scopeOf(toURN) {
			case "user":
				bucket = &totals.User
			case "agent":
				bucket = &totals.Agent
			default:
				bucket = &totals.Other
			}
		}
		bucket.Total++
		if readAt == "" && canceledAt == "" {
			bucket.Unread++
		}
		if archivedAt != "" {
			bucket.Archived++
		}
	}
	return totals, rows.Err()
}

func (s *appServer) handleMessageGroupsList(w http.ResponseWriter, r *http.Request) {
	db, err := s.openStateDB()
	if errors.Is(err, errStateDBUnset) {
		writeJSON(w, http.StatusOK, groupsResponse{Groups: []groupDTO{}})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, groupsResponse{Error: err.Error()})
		return
	}
	defer db.Close()

	regStore := registry.NewStorage(db.DB())
	groups, err := regStore.Search(r.Context(), registry.KindGroup, registry.Filter{Status: registry.StatusAny})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, groupsResponse{Error: err.Error()})
		return
	}

	out := make([]groupDTO, 0, len(groups))
	for _, g := range groups {
		members, err := regStore.ListMembers(r.Context(), g.URN)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, groupsResponse{Error: err.Error()})
			return
		}
		msgs, err := regStore.ListGroupMessages(r.Context(), g.URN, -1, "", 200, time.Time{})
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, groupsResponse{Error: err.Error()})
			return
		}
		out = append(out, groupToDTO(g, members, msgs))
	}

	sort.Slice(out, func(i, j int) bool {
		left := out[i].updatedAt
		right := out[j].updatedAt
		if !left.Equal(right) {
			return left.After(right)
		}
		return out[i].DisplayName < out[j].DisplayName
	})
	writeJSON(w, http.StatusOK, groupsResponse{Groups: out})
}

func (s *appServer) handleMessageGroupReply(w http.ResponseWriter, r *http.Request) {
	var req groupReplyRequest
	if err := decodeJSONBody(r, &req, false); err != nil {
		writeJSON(w, http.StatusBadRequest, groupsResponse{Error: "invalid body: " + err.Error()})
		return
	}
	if req.GroupURN == "" || req.From == "" || strings.TrimSpace(req.Body) == "" {
		writeJSON(w, http.StatusBadRequest, groupsResponse{Error: "group_urn, from, and body are required"})
		return
	}
	kind := req.Kind
	if kind == "" {
		kind = "message"
	}
	contentType := req.ContentType
	if contentType == "" {
		contentType = "text/plain"
	}
	payload, _ := json.Marshal(strings.TrimSpace(req.Body))

	db, err := s.openStateDB()
	if errors.Is(err, errStateDBUnset) {
		writeJSON(w, http.StatusServiceUnavailable, groupsResponse{Error: "no state db configured"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, groupsResponse{Error: err.Error()})
		return
	}
	defer db.Close()

	regStore := registry.NewStorage(db.DB())
	svc := registry.NewService(regStore)
	sent, err := svc.SendToGroup(r.Context(), req.GroupURN, req.From, kind, req.ThreadID, contentType, payload)
	if err != nil {
		writeRegistryActionError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, groupMessageToDTO(sent))
}

func writeRegistryActionError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, registry.ErrInvalidRequest):
		writeJSON(w, http.StatusBadRequest, groupsResponse{Error: err.Error()})
	case errors.Is(err, registry.ErrForbidden):
		writeJSON(w, http.StatusForbidden, groupsResponse{Error: err.Error()})
	case errors.Is(err, registry.ErrGroupArchived):
		writeJSON(w, http.StatusLocked, groupsResponse{Error: err.Error()})
	case errors.Is(err, registry.ErrNotFound):
		writeJSON(w, http.StatusNotFound, groupsResponse{Error: err.Error()})
	default:
		writeJSON(w, http.StatusInternalServerError, groupsResponse{Error: err.Error()})
	}
}

// handleMessageReply sends a new envelope. Reply is non-destructive — it
// POSTs through the messaging store's Send path (cf. CW-20260517-0003,
// which adds the read/delete semantics this endpoint deliberately omits).
func (s *appServer) handleMessageReply(w http.ResponseWriter, r *http.Request) {
	var req replyRequest
	if err := decodeJSONBody(r, &req, false); err != nil {
		writeJSON(w, http.StatusBadRequest, messagesResponse{Error: "invalid body: " + err.Error()})
		return
	}
	from, err := messaging.ParseURN(req.From)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, messagesResponse{Error: "invalid from urn: " + req.From})
		return
	}
	to, err := messaging.ParseURN(req.To)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, messagesResponse{Error: "invalid to urn: " + req.To})
		return
	}
	kind := messaging.Kind(req.Kind)
	if kind == "" {
		kind = messaging.MsgKindResponse
	}

	db, err := s.openStateDB()
	if errors.Is(err, errStateDBUnset) {
		writeJSON(w, http.StatusServiceUnavailable, messagesResponse{Error: "no state db configured"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, messagesResponse{Error: err.Error()})
		return
	}
	defer db.Close()

	env := messaging.Envelope{
		Kind:        kind,
		From:        from,
		To:          to,
		InReplyTo:   req.InReplyTo,
		ThreadID:    req.ThreadID,
		ContentType: req.ContentType,
	}
	if req.Body != "" {
		body, _ := json.Marshal(req.Body)
		env.Payload = body
		if env.ContentType == "" {
			env.ContentType = "text/plain"
		}
	}
	sent, err := db.MessagingStore().Send(r.Context(), env)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, messagesResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, messageDTO{
		ID:          sent.ID,
		Kind:        string(sent.Kind),
		Channel:     string(sent.Channel),
		From:        sent.From.URN(),
		To:          sent.To.URN(),
		ThreadID:    sent.ThreadID,
		InReplyTo:   sent.InReplyTo,
		Body:        payloadBody(string(sent.Payload)),
		ContentType: sent.ContentType,
		Scope:       scopeOf(sent.To.URN()),
		CreatedAt:   sent.CreatedAt.Format(time.RFC3339Nano),
	})
}

// recipientActionRequest is the POST body for the recipient-scoped,
// idempotent message state transitions (archive / mark-read).
type recipientActionRequest struct {
	ID string `json:"id"`
	As string `json:"as"` // recipient URN
}

// handleMessageArchive soft-deletes (archives) a message for its recipient.
func (s *appServer) handleMessageArchive(w http.ResponseWriter, r *http.Request) {
	s.messageRecipientAction(w, r, store.InboxStore.Archive)
}

// handleMessageMarkRead marks a message read by its recipient (idempotent).
func (s *appServer) handleMessageMarkRead(w http.ResponseWriter, r *http.Request) {
	s.messageRecipientAction(w, r, store.InboxStore.MarkRead)
}

// messageRecipientAction runs an idempotent (id, recipient)-scoped message
// transition — Archive or MarkRead — from a POST {id, as} body, mapping the
// store's not-found / wrong-recipient errors to 404 / 409.
func (s *appServer) messageRecipientAction(w http.ResponseWriter, r *http.Request,
	action func(store.InboxStore, context.Context, string, messaging.Address) error) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	var req recipientActionRequest
	if err := decodeJSONBody(r, &req, false); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body: " + err.Error()})
		return
	}
	if req.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id required"})
		return
	}
	recipient, err := messaging.ParseURN(req.As)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid recipient urn: " + req.As})
		return
	}

	db, err := s.openStateDB()
	if errors.Is(err, errStateDBUnset) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no state db configured"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer db.Close()

	if err := action(db.MessagingStore(), r.Context(), req.ID, recipient); err != nil {
		switch {
		case errors.Is(err, messaging.ErrNotFound):
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "message not found"})
		case errors.Is(err, store.ErrWrongRecipient):
			writeJSON(w, http.StatusConflict, map[string]string{"error": "not the intended recipient"})
		default:
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ─── Activity ────────────────────────────────────────────────────────────────

type eventsResponse struct {
	Events []eventDTO `json:"events"`
	Total  int        `json:"total"`
	Error  string     `json:"error,omitempty"`
}

type eventDTO struct {
	Seq       int64  `json:"seq"`
	At        string `json:"at"`
	Scope     string `json:"scope"`
	SessionID string `json:"session_id,omitempty"`
	Kind      string `json:"kind"`
	Payload   string `json:"payload,omitempty"`
}

type toolCallsResponse struct {
	ToolCalls []toolCallDTO `json:"tool_calls"`
	Total     int           `json:"total"`
	Error     string        `json:"error,omitempty"`
}

type toolCallDTO struct {
	ID           int64  `json:"id"`
	SessionID    string `json:"session_id,omitempty"`
	Server       string `json:"server,omitempty"`
	ToolName     string `json:"tool_name"`
	ArgsSchemaFP string `json:"args_schema_fp,omitempty"`
	DurationMs   int64  `json:"duration_ms"`
	OK           bool   `json:"ok"`
	Error        string `json:"error,omitempty"`
	Timestamp    string `json:"timestamp"`
	Payload      string `json:"payload,omitempty"`
}

func (s *appServer) handleActivityEvents(w http.ResponseWriter, _ *http.Request) {
	db, err := s.openStateDB()
	if errors.Is(err, errStateDBUnset) {
		writeJSON(w, http.StatusOK, eventsResponse{Events: []eventDTO{}})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, eventsResponse{Error: err.Error()})
		return
	}
	defer db.Close()

	total, err := db.CountEvents()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, eventsResponse{Error: err.Error()})
		return
	}
	rows, err := db.ListRecentEvents(500)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, eventsResponse{Error: err.Error()})
		return
	}
	out := make([]eventDTO, 0, len(rows))
	for _, e := range rows {
		out = append(out, eventDTO{
			Seq:       e.Seq,
			At:        e.At.Format(time.RFC3339Nano),
			Scope:     e.Scope,
			SessionID: e.SessionID,
			Kind:      e.Kind,
			Payload:   e.PayloadJSON,
		})
	}
	writeJSON(w, http.StatusOK, eventsResponse{Events: out, Total: total})
}

func (s *appServer) handleActivityToolCalls(w http.ResponseWriter, _ *http.Request) {
	db, err := s.openStateDB()
	if errors.Is(err, errStateDBUnset) {
		writeJSON(w, http.StatusOK, toolCallsResponse{ToolCalls: []toolCallDTO{}})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, toolCallsResponse{Error: err.Error()})
		return
	}
	defer db.Close()

	total, err := db.CountProxyEvents(store.ProxyEventFilter{})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, toolCallsResponse{Error: err.Error()})
		return
	}
	rows, err := db.QueryProxyEvents(store.ProxyEventFilter{Limit: 500})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, toolCallsResponse{Error: err.Error()})
		return
	}
	// QueryProxyEvents returns oldest-first; reverse for newest-first.
	out := make([]toolCallDTO, 0, len(rows))
	for i := len(rows) - 1; i >= 0; i-- {
		ev := rows[i]
		payload := toolCallPayload(ev)
		out = append(out, toolCallDTO{
			ID:           ev.ID,
			SessionID:    ev.SessionID,
			Server:       ev.Server,
			ToolName:     ev.ToolName,
			ArgsSchemaFP: ev.ArgsSchemaFP,
			DurationMs:   ev.DurationMs,
			OK:           ev.OK,
			Error:        ev.Error,
			Timestamp:    ev.Timestamp.Format(time.RFC3339Nano),
			Payload:      payload,
		})
	}
	writeJSON(w, http.StatusOK, toolCallsResponse{ToolCalls: out, Total: total})
}

func toolCallPayload(ev store.ProxyEvent) string {
	raw, err := json.Marshal(events.ToolCallEvent{
		SessionID:    ev.SessionID,
		ToolName:     ev.ToolName,
		Server:       ev.Server,
		ArgsSchemaFP: ev.ArgsSchemaFP,
		DurationMs:   ev.DurationMs,
		OK:           ev.OK,
		Error:        ev.Error,
		Timestamp:    ev.Timestamp,
	})
	if err != nil {
		return ""
	}
	return string(raw)
}

// ─── Overview ────────────────────────────────────────────────────────────────

type nameCount struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

type overviewResponse struct {
	Sessions  overviewSessions  `json:"sessions"`
	ToolCalls overviewToolCalls `json:"tool_calls"`
	Messages  overviewMessages  `json:"messages"`
	Events    overviewEvents    `json:"events"`
	AI        overviewAI        `json:"ai"`
	Catalog   overviewCatalog   `json:"catalog"`
	Health    healthResponse    `json:"health"`
	Error     string            `json:"error,omitempty"`
}

type overviewSessions struct {
	Total      int         `json:"total"`
	Running    int         `json:"running"`
	Ended      int         `json:"ended"`
	SuccessPct int         `json:"success_pct"`
	FailurePct int         `json:"failure_pct"`
	AvgSeconds int         `json:"avg_seconds"`
	Recent24h  int         `json:"recent_24h"`
	Trend      []int       `json:"trend"`
	ByState    []nameCount `json:"by_state"`
	ByProvider []nameCount `json:"by_provider"`
	ByProject  []nameCount `json:"by_project"`
}

type overviewToolCalls struct {
	Total      int         `json:"total"`
	OK         int         `json:"ok"`
	Errors     int         `json:"errors"`
	SuccessPct int         `json:"success_pct"`
	P50ms      int64       `json:"p50_ms"`
	P95ms      int64       `json:"p95_ms"`
	AvgMs      int64       `json:"avg_ms"`
	Recent1h   int         `json:"recent_1h"`
	SlowCalls  int         `json:"slow_calls"`
	Sessions   int         `json:"sessions"`
	TopTools   []nameCount `json:"top_tools"`
	TopErrors  []nameCount `json:"top_errors"`
	ByServer   []nameCount `json:"by_server"`
	Latency    []nameCount `json:"latency"`
	Trend      []int       `json:"trend"`
}

type overviewMessages struct {
	Total     int         `json:"total"`
	Unread    int         `json:"unread"`
	Archived  int         `json:"archived"`
	Recent24h int         `json:"recent_24h"`
	ByKind    []nameCount `json:"by_kind"`
	ByScope   []nameCount `json:"by_scope"`
	Trend     []int       `json:"trend"`
}

type overviewEvents struct {
	Total     int         `json:"total"`
	Recent1h  int         `json:"recent_1h"`
	LatestSeq int64       `json:"latest_seq"`
	ByScope   []nameCount `json:"by_scope"`
	ByKind    []nameCount `json:"by_kind"`
	Trend     []int       `json:"trend"`
}

type overviewAI struct {
	ConfiguredProviders int         `json:"configured_providers"`
	EnabledProviders    int         `json:"enabled_providers"`
	Routes              int         `json:"routes"`
	Requests            int         `json:"requests"`
	Successes           int         `json:"successes"`
	Errors              int         `json:"errors"`
	BudgetRejections    int         `json:"budget_rejections"`
	InputTokens         int         `json:"input_tokens"`
	OutputTokens        int         `json:"output_tokens"`
	EstimatedCostUSD    float64     `json:"estimated_cost_usd"`
	ByProvider          []nameCount `json:"by_provider"`
	ByModel             []nameCount `json:"by_model"`
	ByEventType         []nameCount `json:"by_event_type"`
	Trend               []int       `json:"trend"`
}

type overviewCatalog struct {
	Projects  int `json:"projects"`
	Agents    int `json:"agents"`
	Providers int `json:"providers"`
	Launches  int `json:"launches"`
}

const overviewTrendBuckets = 24

// handleOverview aggregates recent DB activity into a single dashboard
// payload. Catalog/health come from the catalog; the rest from the state
// DB. A missing state DB yields zeroed stats rather than an error.
func (s *appServer) handleOverview(w http.ResponseWriter, _ *http.Request) {
	resp := overviewResponse{Health: healthResponse{Status: "ok", CatalogRoot: s.catalogRoot}}
	if _, err := os.Stat(s.catalogRoot); err != nil {
		resp.Health.Status = "degraded"
		resp.Health.Error = err.Error()
	}
	if cat, err := config.Load(s.catalogRoot); err == nil {
		resp.Catalog = overviewCatalog{
			Projects:  len(cat.Projects),
			Agents:    len(cat.Agents),
			Providers: len(cat.Providers),
			Launches:  len(cat.Launches),
		}
		resp.AI.ConfiguredProviders = len(cat.Global.AI.Providers)
		resp.AI.Routes = len(cat.Global.AI.Routing.Routes)
		for _, provider := range cat.Global.AI.Providers {
			if provider.Enabled {
				resp.AI.EnabledProviders++
			}
		}
	}

	db, err := s.openStateDB()
	if errors.Is(err, errStateDBUnset) {
		writeJSON(w, http.StatusOK, resp)
		return
	}
	if err != nil {
		resp.Error = err.Error()
		writeJSON(w, http.StatusOK, resp)
		return
	}
	defer db.Close()

	populateOverviewSessions(db, &resp)

	if total, terr := db.CountProxyEvents(store.ProxyEventFilter{}); terr == nil {
		resp.ToolCalls.Total = total
	}
	if errors, eerr := db.CountProxyEvents(store.ProxyEventFilter{ErrorsOnly: true}); eerr == nil {
		resp.ToolCalls.Errors = errors
		resp.ToolCalls.OK = resp.ToolCalls.Total - errors
		if resp.ToolCalls.Total > 0 {
			resp.ToolCalls.SuccessPct = resp.ToolCalls.OK * 100 / resp.ToolCalls.Total
		}
	}
	if proxy, perr := db.QueryProxyEvents(store.ProxyEventFilter{Limit: -1}); perr == nil {
		toolCounts := map[string]int{}
		errCounts := map[string]int{}
		serverCounts := map[string]int{}
		sessionSet := map[string]struct{}{}
		latencyCounts := map[string]int{}
		var durs []int64
		var times []time.Time
		var sum int64
		cutoff := time.Now().UTC().Add(-time.Hour)
		for _, ev := range proxy {
			toolCounts[ev.ToolName]++
			server := ev.Server
			if server == "" {
				server = "native"
			}
			serverCounts[server]++
			if ev.SessionID != "" {
				sessionSet[ev.SessionID] = struct{}{}
			}
			durs = append(durs, ev.DurationMs)
			sum += ev.DurationMs
			times = append(times, ev.Timestamp)
			if ev.Timestamp.After(cutoff) {
				resp.ToolCalls.Recent1h++
			}
			if ev.DurationMs >= 1000 {
				resp.ToolCalls.SlowCalls++
			}
			latencyCounts[latencyBand(ev.DurationMs)]++
			if !ev.OK {
				errCounts[ev.ToolName]++
			}
		}
		sort.Slice(durs, func(i, j int) bool { return durs[i] < durs[j] })
		resp.ToolCalls.P50ms = pctile(durs, 0.50)
		resp.ToolCalls.P95ms = pctile(durs, 0.95)
		if len(durs) > 0 {
			resp.ToolCalls.AvgMs = sum / int64(len(durs))
		}
		resp.ToolCalls.Sessions = len(sessionSet)
		resp.ToolCalls.TopTools = topN(toolCounts, 5)
		resp.ToolCalls.TopErrors = topN(errCounts, 5)
		resp.ToolCalls.ByServer = topN(serverCounts, 6)
		resp.ToolCalls.Latency = orderedLatencyCounts(latencyCounts)
		resp.ToolCalls.Trend = bucketCounts(times, overviewTrendBuckets)
	}

	populateOverviewMessages(db, &resp)

	if total, terr := db.CountEvents(); terr == nil {
		resp.Events.Total = total
	}
	if evs, eerr := db.ListRecentEvents(1000); eerr == nil {
		scopeCounts := map[string]int{}
		kindCounts := map[string]int{}
		var times []time.Time
		cutoff := time.Now().UTC().Add(-time.Hour)
		for _, e := range evs {
			scopeCounts[e.Scope]++
			kindCounts[e.Kind]++
			times = append(times, e.At)
			if e.At.After(cutoff) {
				resp.Events.Recent1h++
			}
			if e.Seq > resp.Events.LatestSeq {
				resp.Events.LatestSeq = e.Seq
			}
		}
		resp.Events.ByScope = topN(scopeCounts, 6)
		resp.Events.ByKind = topN(kindCounts, 8)
		resp.Events.Trend = bucketCounts(times, overviewTrendBuckets)
	}

	populateOverviewAI(db, &resp)

	writeJSON(w, http.StatusOK, resp)
}

func populateOverviewSessions(db *store.Store, resp *overviewResponse) {
	rows, err := db.DB().Query(
		`SELECT state, provider_id, project_id, created_at,
		        COALESCE(ended_at, ''), exit_code
		   FROM sessions`)
	if err != nil {
		return
	}
	defer rows.Close()

	stateCounts := map[string]int{}
	providerCounts := map[string]int{}
	projectCounts := map[string]int{}
	var starts []time.Time
	var durations []float64
	successes := 0
	failures := 0
	cutoff := time.Now().UTC().Add(-24 * time.Hour)

	for rows.Next() {
		var state, providerID, projectID, createdAt, endedAt string
		var exitCode sql.NullInt64
		if err := rows.Scan(&state, &providerID, &projectID, &createdAt, &endedAt, &exitCode); err != nil {
			return
		}
		resp.Sessions.Total++
		stateCounts[state]++
		providerCounts[valueOr(providerID, "unknown")]++
		projectCounts[valueOr(projectID, "unknown")]++
		if state == "running" {
			resp.Sessions.Running++
		}
		if c, ok := parseTime(createdAt); ok {
			starts = append(starts, c)
			if c.After(cutoff) {
				resp.Sessions.Recent24h++
			}
			if endedAt != "" {
				if e, ok := parseTime(endedAt); ok {
					durations = append(durations, e.Sub(c).Seconds())
				}
			}
		}
		if endedAt != "" {
			resp.Sessions.Ended++
			if exitCode.Valid && exitCode.Int64 == 0 {
				successes++
			} else {
				failures++
			}
		}
	}
	if resp.Sessions.Ended > 0 {
		resp.Sessions.SuccessPct = successes * 100 / resp.Sessions.Ended
		resp.Sessions.FailurePct = failures * 100 / resp.Sessions.Ended
	}
	if len(durations) > 0 {
		var sum float64
		for _, d := range durations {
			sum += d
		}
		resp.Sessions.AvgSeconds = int(sum / float64(len(durations)))
	}
	resp.Sessions.ByState = topN(stateCounts, 6)
	resp.Sessions.ByProvider = topN(providerCounts, 6)
	resp.Sessions.ByProject = topN(projectCounts, 6)
	resp.Sessions.Trend = bucketCounts(starts, overviewTrendBuckets)
}

func populateOverviewMessages(db *store.Store, resp *overviewResponse) {
	rows, err := db.DB().Query(
		`SELECT kind, to_urn, created_at, COALESCE(read_at, ''),
		        COALESCE(archived_at, ''), COALESCE(canceled_at, ''),
		        COALESCE(group_urn, '')
		   FROM messages`)
	if err != nil {
		return
	}
	defer rows.Close()

	kindCounts := map[string]int{}
	scopeCounts := map[string]int{}
	var times []time.Time
	cutoff := time.Now().UTC().Add(-24 * time.Hour)

	for rows.Next() {
		var kind, toURN, createdAt, readAt, archivedAt, canceledAt, groupURN string
		if err := rows.Scan(&kind, &toURN, &createdAt, &readAt, &archivedAt, &canceledAt, &groupURN); err != nil {
			return
		}
		resp.Messages.Total++
		kindCounts[kind]++
		scope := scopeOf(toURN)
		if groupURN != "" {
			scope = "group"
		}
		scopeCounts[scope]++
		if readAt == "" && canceledAt == "" {
			resp.Messages.Unread++
		}
		if archivedAt != "" {
			resp.Messages.Archived++
		}
		if t, ok := parseTime(createdAt); ok {
			times = append(times, t)
			if t.After(cutoff) {
				resp.Messages.Recent24h++
			}
		}
	}
	resp.Messages.ByKind = topN(kindCounts, 6)
	resp.Messages.ByScope = topN(scopeCounts, 6)
	resp.Messages.Trend = bucketCounts(times, overviewTrendBuckets)
}

func populateOverviewAI(db *store.Store, resp *overviewResponse) {
	summary, err := db.QueryAIUsageSummary(store.AIUsageFilter{})
	if err == nil {
		resp.AI.Requests = summary.Requests
		resp.AI.Successes = summary.Successes
		resp.AI.Errors = summary.Errors
		resp.AI.InputTokens = summary.InputTokens
		resp.AI.OutputTokens = summary.OutputTokens
		resp.AI.EstimatedCostUSD = summary.EstimatedCostUSD
		resp.AI.ByProvider = topN(aiBreakdownCounts(summary.ByProvider), 6)
		resp.AI.ByModel = topN(aiBreakdownCounts(summary.ByModel), 6)
	}
	if events, err := db.QueryAIEvents(store.AIEventFilter{Limit: -1}); err == nil {
		eventTypeCounts := map[string]int{}
		var times []time.Time
		for _, ev := range events {
			eventTypeCounts[valueOr(ev.EventType, "unknown")]++
			times = append(times, ev.Timestamp)
			if ev.EventType == "budget_rejection" {
				resp.AI.BudgetRejections++
			}
		}
		resp.AI.ByEventType = topN(eventTypeCounts, 8)
		resp.AI.Trend = bucketCounts(times, overviewTrendBuckets)
	}
}

func aiBreakdownCounts(rows []store.AIUsageBreakdown) map[string]int {
	out := make(map[string]int, len(rows))
	for _, row := range rows {
		out[row.Key] = row.Requests
	}
	return out
}

// ─── Session detail ──────────────────────────────────────────────────────────

type attachmentDTO struct {
	ID         string `json:"id"`
	ClientKind string `json:"client_kind"`
	AttachedAt string `json:"attached_at"`
	DetachedAt string `json:"detached_at,omitempty"`
}

type checkpointDTO struct {
	ID                 string `json:"id"`
	Status             string `json:"status,omitempty"`
	TaskID             string `json:"task_id,omitempty"`
	WorkflowID         string `json:"workflow_id,omitempty"`
	Summary            string `json:"summary,omitempty"`
	CompletedWork      string `json:"completed_work,omitempty"`
	PendingWork        string `json:"pending_work,omitempty"`
	KeyDecisions       string `json:"key_decisions,omitempty"`
	NextRecommendation string `json:"next_recommendation,omitempty"`
	CreatedAt          string `json:"created_at"`
	SourceSessionID    string `json:"source_session_id,omitempty"`
}

type sessionDetailResponse struct {
	Session     sessionDTO      `json:"session"`
	GroupID     string          `json:"group_id,omitempty"`
	Events      []eventDTO      `json:"events"`
	Attachments []attachmentDTO `json:"attachments"`
	LaunchPlan  string          `json:"launch_plan,omitempty"`
	Checkpoints []checkpointDTO `json:"checkpoints"`
	Error       string          `json:"error,omitempty"`
}

// handleSessionDetail returns a composite view of one session: its row,
// per-session events, client attachments, launch plan, and the checkpoints
// of its logical agent.
func (s *appServer) handleSessionDetail(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, sessionDetailResponse{Error: "id query param required"})
		return
	}
	db, err := s.openStateDB()
	if errors.Is(err, errStateDBUnset) {
		writeJSON(w, http.StatusServiceUnavailable, sessionDetailResponse{Error: "no state db configured"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, sessionDetailResponse{Error: err.Error()})
		return
	}
	defer db.Close()

	row, err := db.GetSession(id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, sessionDetailResponse{Error: err.Error()})
		return
	}
	if row == nil {
		writeJSON(w, http.StatusNotFound, sessionDetailResponse{Error: "session not found"})
		return
	}

	resp := sessionDetailResponse{
		Session: sessionDTO{
			ID:             row.ID,
			LaunchID:       row.LaunchID,
			ProjectID:      row.ProjectID,
			LogicalAgentID: row.LogicalAgentID,
			ProviderID:     row.ProviderID,
			ProviderKind:   row.ProviderKind,
			Workspace:      row.Workspace,
			State:          row.State,
			PID:            nullableInt(row.PID.Valid, int(row.PID.Int64)),
			ExitCode:       nullableInt(row.ExitCode.Valid, int(row.ExitCode.Int64)),
			CreatedAt:      row.CreatedAt,
			UpdatedAt:      row.UpdatedAt,
			EndedAt:        nullableString(row.EndedAt.Valid, row.EndedAt.String),
		},
		Events:      []eventDTO{},
		Attachments: []attachmentDTO{},
		Checkpoints: []checkpointDTO{},
	}
	if row.SessionGroupID.Valid {
		resp.GroupID = row.SessionGroupID.String
	}

	if evs, eerr := db.ListEventsBySession(id, 200, 0); eerr == nil {
		for _, e := range evs {
			resp.Events = append(resp.Events, eventDTO{
				Seq:       e.Seq,
				At:        e.At.Format(time.RFC3339Nano),
				Scope:     e.Scope,
				SessionID: e.SessionID,
				Kind:      e.Kind,
				Payload:   e.PayloadJSON,
			})
		}
	}

	if atts, aerr := db.ListClientAttachments(id); aerr == nil {
		for _, a := range atts {
			resp.Attachments = append(resp.Attachments, attachmentDTO{
				ID:         a.ID,
				ClientKind: a.ClientKind,
				AttachedAt: a.AttachedAt,
				DetachedAt: nullableString(a.DetachedAt.Valid, a.DetachedAt.String),
			})
		}
	}

	if plan, perr := db.GetLaunchPlan(id); perr == nil && plan != nil {
		if b, merr := json.MarshalIndent(plan, "", "  "); merr == nil {
			resp.LaunchPlan = string(b)
		}
	}

	if row.LogicalAgentID != "" {
		if cps, cerr := db.ListCheckpointsByLogicalAgent(row.LogicalAgentID); cerr == nil {
			for _, c := range cps {
				resp.Checkpoints = append(resp.Checkpoints, checkpointDTO{
					ID:                 c.ID,
					Status:             c.Status,
					TaskID:             c.TaskID,
					WorkflowID:         c.WorkflowID,
					Summary:            c.Summary,
					CompletedWork:      c.CompletedWork,
					PendingWork:        c.PendingWork,
					KeyDecisions:       c.KeyDecisions,
					NextRecommendation: c.NextRecommendation,
					CreatedAt:          c.CreatedAt,
					SourceSessionID:    c.SourceSessionID,
				})
			}
		}
	}

	writeJSON(w, http.StatusOK, resp)
}

// ─── Aggregate helpers ───────────────────────────────────────────────────────

// parseTime parses an RFC3339(Nano) timestamp, tolerating either precision.
func parseTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, true
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, true
	}
	return time.Time{}, false
}

// pctile returns the p-quantile (0..1) of an already-sorted slice.
func pctile(sorted []int64, p float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(float64(len(sorted)-1) * p)
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// topN returns the n highest-count entries, ties broken by name.
func topN(counts map[string]int, n int) []nameCount {
	out := make([]nameCount, 0, len(counts))
	for k, v := range counts {
		out = append(out, nameCount{Name: k, Count: v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Name < out[j].Name
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}

func valueOr(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

func latencyBand(ms int64) string {
	switch {
	case ms < 10:
		return "<10ms"
	case ms < 100:
		return "10-99ms"
	case ms < 500:
		return "100-499ms"
	case ms < 1000:
		return "500-999ms"
	default:
		return ">=1s"
	}
}

func orderedLatencyCounts(counts map[string]int) []nameCount {
	order := []string{"<10ms", "10-99ms", "100-499ms", "500-999ms", ">=1s"}
	out := make([]nameCount, 0, len(order))
	for _, name := range order {
		if counts[name] > 0 {
			out = append(out, nameCount{Name: name, Count: counts[name]})
		}
	}
	return out
}

// bucketCounts distributes timestamps into n equal-width buckets spanning
// [min,max], returned oldest→newest — a sparkline-ready series.
func bucketCounts(times []time.Time, n int) []int {
	buckets := make([]int, n)
	if len(times) == 0 || n <= 0 {
		return buckets
	}
	lo, hi := times[0], times[0]
	for _, t := range times {
		if t.Before(lo) {
			lo = t
		}
		if t.After(hi) {
			hi = t
		}
	}
	span := hi.Sub(lo)
	if span <= 0 {
		buckets[n-1] = len(times)
		return buckets
	}
	for _, t := range times {
		idx := int(float64(t.Sub(lo)) / float64(span) * float64(n))
		if idx >= n {
			idx = n - 1
		}
		if idx < 0 {
			idx = 0
		}
		buckets[idx]++
	}
	return buckets
}

// ─── MCP ─────────────────────────────────────────────────────────────────────

type mcpServerDTO struct {
	ID          string   `json:"id"`
	Transport   string   `json:"transport"`
	Command     string   `json:"command,omitempty"`
	Args        []string `json:"args,omitempty"`
	URL         string   `json:"url,omitempty"`
	EnvKeys     []string `json:"env_keys,omitempty"`
	HasToken    bool     `json:"has_token"`
	Scopes      []string `json:"scopes,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	Enabled     bool     `json:"enabled"`
	Visibility  string   `json:"visibility"`
	ProjectRefs []string `json:"project_refs,omitempty"`
	LaunchRefs  []string `json:"launch_refs,omitempty"`
}

type mcpServersResponse struct {
	Servers []mcpServerDTO `json:"servers"`
	Error   string         `json:"error,omitempty"`
}

// handleMCPServers lists the upstream MCP servers from the catalog
// (<catalog>/mcp-servers/*.yaml). Token is redacted to a boolean and only
// env keys are returned, since both can carry secrets.
func (s *appServer) handleMCPServers(w http.ResponseWriter, _ *http.Request) {
	entries, err := config.LoadMCPServerCatalog(s.catalogRoot)
	if err != nil {
		writeJSON(w, http.StatusOK, mcpServersResponse{Servers: []mcpServerDTO{}, Error: err.Error()})
		return
	}
	cat, err := config.Load(s.catalogRoot)
	if err != nil {
		writeJSON(w, http.StatusOK, mcpServersResponse{Servers: []mcpServerDTO{}, Error: err.Error()})
		return
	}
	projectRefsByServer := make(map[string][]string)
	for _, project := range cat.Projects {
		for _, serverID := range project.MCP.Servers {
			projectRefsByServer[serverID] = append(projectRefsByServer[serverID], project.ID)
		}
	}
	launchRefsByServer := make(map[string][]string)
	for _, launch := range cat.Launches {
		for _, serverID := range launch.MCP.Servers {
			launchRefsByServer[serverID] = append(launchRefsByServer[serverID], launch.ID)
		}
	}
	out := make([]mcpServerDTO, 0, len(entries))
	for _, e := range entries {
		envKeys := make([]string, 0, len(e.Env))
		for k := range e.Env {
			envKeys = append(envKeys, k)
		}
		sort.Strings(envKeys)
		projectRefs := append([]string(nil), projectRefsByServer[e.ID]...)
		launchRefs := append([]string(nil), launchRefsByServer[e.ID]...)
		sort.Strings(projectRefs)
		sort.Strings(launchRefs)
		enabled := e.Enabled == nil || *e.Enabled
		visibility := "catalog_available"
		switch {
		case !enabled:
			visibility = "disabled"
		case len(launchRefs) > 0:
			visibility = "launch_allowlist"
		case len(projectRefs) > 0:
			visibility = "project_allowlist"
		}
		out = append(out, mcpServerDTO{
			ID:          e.ID,
			Transport:   e.Transport,
			Command:     e.Command,
			Args:        e.Args,
			URL:         e.URL,
			EnvKeys:     envKeys,
			HasToken:    e.Token != "",
			Scopes:      e.Scopes,
			Tags:        e.Tags,
			Enabled:     enabled,
			Visibility:  visibility,
			ProjectRefs: projectRefs,
			LaunchRefs:  launchRefs,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	writeJSON(w, http.StatusOK, mcpServersResponse{Servers: out})
}

func (s *appServer) handleMCPServerSave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, actionResponse{Error: "method not allowed"})
		return
	}
	var req mcpServerSaveRequest
	if err := decodeJSONBody(r, &req, false); err != nil {
		writeJSON(w, http.StatusBadRequest, actionResponse{Error: "invalid request body: " + err.Error()})
		return
	}
	req.ID = strings.TrimSpace(req.ID)
	if err := validateMCPServerSave(req); err != nil {
		writeJSON(w, http.StatusBadRequest, actionResponse{Error: err.Error()})
		return
	}
	raw, path, found, err := s.rawMCPServer(req.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, actionResponse{Error: err.Error()})
		return
	}
	if !found {
		path = filepath.Join(s.catalogRoot, "mcp-servers", req.ID+".yaml")
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	} else if found && raw.Enabled != nil {
		enabled = *raw.Enabled
	}
	raw.ID = req.ID
	raw.Transport = req.Transport
	raw.Command = req.Command
	raw.Args = req.Args
	raw.URL = req.URL
	if req.Token != "" {
		switch req.Token {
		case secretPreserveMarker:
		case secretDeleteMarker:
			raw.Token = ""
		default:
			raw.Token = req.Token
		}
	} else if !found {
		raw.Token = ""
	}
	if req.Env != nil {
		raw.Env = mergeMCPEnv(raw.Env, req.Env)
	}
	raw.Scopes = req.Scopes
	raw.Tags = req.Tags
	raw.Enabled = &enabled
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		writeJSON(w, http.StatusInternalServerError, actionResponse{Error: err.Error()})
		return
	}
	backupPath, err := writeCatalogYAMLFile(path, raw, 0o600)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, actionResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, actionResponse{Status: "saved", BackupPath: backupPath})
}

func (s *appServer) handleMCPServerDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, actionResponse{Error: "method not allowed"})
		return
	}
	var req mcpServerIDRequest
	if err := decodeJSONBody(r, &req, false); err != nil {
		writeJSON(w, http.StatusBadRequest, actionResponse{Error: "invalid request body: " + err.Error()})
		return
	}
	id := strings.TrimSpace(req.ID)
	if !validCatalogID(id) {
		writeJSON(w, http.StatusBadRequest, actionResponse{Error: "invalid id"})
		return
	}
	_, path, found, err := s.rawMCPServer(id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, actionResponse{Error: err.Error()})
		return
	}
	if !found {
		writeJSON(w, http.StatusNotFound, actionResponse{Error: "server not found"})
		return
	}
	backupPath, err := removeCatalogFileWithBackup(path)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, actionResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, actionResponse{Status: "deleted", BackupPath: backupPath})
}

func (s *appServer) handleMCPServerToggle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, actionResponse{Error: "method not allowed"})
		return
	}
	var req mcpServerIDRequest
	if err := decodeJSONBody(r, &req, false); err != nil {
		writeJSON(w, http.StatusBadRequest, actionResponse{Error: "invalid request body: " + err.Error()})
		return
	}
	id := strings.TrimSpace(req.ID)
	if !validCatalogID(id) {
		writeJSON(w, http.StatusBadRequest, actionResponse{Error: "invalid id"})
		return
	}
	if req.Enabled == nil {
		writeJSON(w, http.StatusBadRequest, actionResponse{Error: "enabled required"})
		return
	}
	raw, path, found, err := s.rawMCPServer(id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, actionResponse{Error: err.Error()})
		return
	}
	if !found {
		writeJSON(w, http.StatusNotFound, actionResponse{Error: "server not found"})
		return
	}
	raw.Enabled = req.Enabled
	backupPath, err := writeCatalogYAMLFile(path, raw, 0o600)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, actionResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, actionResponse{Status: "saved", BackupPath: backupPath})
}

type mcpToolDTO struct {
	Name         string `json:"name"`
	Server       string `json:"server"`
	Calls        int    `json:"calls"`
	Errors       int    `json:"errors"`
	SuccessPct   int    `json:"success_pct"`
	AvgMs        int64  `json:"avg_ms"`
	P95ms        int64  `json:"p95_ms"`
	LastSeen     string `json:"last_seen"`
	Live         bool   `json:"live"`
	Source       string `json:"source"`
	ServerStatus string `json:"server_status,omitempty"`
	ServerError  string `json:"server_error,omitempty"`
}

type mcpToolsResponse struct {
	Tools      []mcpToolDTO `json:"tools"`
	TotalCalls int          `json:"total_calls"`
	Error      string       `json:"error,omitempty"`
}

type mcpToolAgg struct {
	server   string
	calls    int
	errors   int
	durs     []int64
	lastSeen time.Time
}

type liveMCPToolRow struct {
	Name         string
	Server       string
	ServerStatus string
	ServerError  string
}

// handleMCPTools returns a merged tool view:
//   - live on-demand upstream MCP inventory via internal/mcpadapter
//   - usage/latency history aggregated from proxy_events
//
// Tools present only in usage history remain visible as stale rows.
func (s *appServer) handleMCPTools(w http.ResponseWriter, _ *http.Request) {
	liveByName, liveStatuses, liveErr := s.liveMCPTools()

	db, err := s.openStateDB()
	if errors.Is(err, errStateDBUnset) {
		writeJSON(w, http.StatusOK, mcpToolsResponse{
			Tools: flattenMCPToolRows(nil, liveByName, liveStatuses),
			Error: errorString(liveErr),
		})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, mcpToolsResponse{Error: err.Error()})
		return
	}
	defer db.Close()

	totalCalls, err := db.CountProxyEvents(store.ProxyEventFilter{})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, mcpToolsResponse{Error: err.Error()})
		return
	}
	events, err := db.QueryProxyEvents(store.ProxyEventFilter{Limit: -1})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, mcpToolsResponse{Error: err.Error()})
		return
	}

	byTool := map[string]*mcpToolAgg{}
	for _, ev := range events {
		a := byTool[ev.ToolName]
		if a == nil {
			a = &mcpToolAgg{}
			byTool[ev.ToolName] = a
		}
		if ev.Server != "" {
			a.server = ev.Server
		}
		a.calls++
		if !ev.OK {
			a.errors++
		}
		a.durs = append(a.durs, ev.DurationMs)
		if ev.Timestamp.After(a.lastSeen) {
			a.lastSeen = ev.Timestamp
		}
	}

	writeJSON(w, http.StatusOK, mcpToolsResponse{
		Tools:      flattenMCPToolRows(byTool, liveByName, liveStatuses),
		TotalCalls: totalCalls,
		Error:      errorString(liveErr),
	})
}

func (s *appServer) liveMCPTools() (map[string]liveMCPToolRow, map[string]mcpadapter.ServerStatus, error) {
	entries, err := config.LoadMCPServers(s.catalogRoot)
	if err != nil {
		return nil, nil, err
	}
	registry := mcpadapter.NewToolRegistry()
	pool := mcpadapter.NewClientPool(entries, registry)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := pool.Start(ctx); err != nil {
		return nil, nil, err
	}
	defer pool.Shutdown()

	statuses := pool.StatusSummary()
	statusByID := make(map[string]mcpadapter.ServerStatus, len(statuses))
	for _, status := range statuses {
		statusByID[status.ID] = status
	}

	out := make(map[string]liveMCPToolRow)
	for _, def := range registry.AllDefinitions() {
		rt, ok := registry.Lookup(def.Name)
		if !ok {
			continue
		}
		row := liveMCPToolRow{
			Name:         def.Name,
			Server:       rt.ServerID,
			ServerStatus: "connected",
		}
		if status, ok := statusByID[rt.ServerID]; ok {
			row.ServerStatus = status.Status
			row.ServerError = status.Error
		}
		out[row.Name] = row
	}
	return out, statusByID, nil
}

func flattenMCPToolRows(usage map[string]*mcpToolAgg, liveByName map[string]liveMCPToolRow, liveStatuses map[string]mcpadapter.ServerStatus) []mcpToolDTO {
	names := make(map[string]struct{}, len(liveByName))
	for name := range liveByName {
		names[name] = struct{}{}
	}
	for name := range usage {
		names[name] = struct{}{}
	}

	out := make([]mcpToolDTO, 0, len(names))
	for name := range names {
		row := mcpToolDTO{
			Name:       name,
			SuccessPct: 0,
			Source:     "usage_only",
		}
		if live, ok := liveByName[name]; ok {
			row.Server = live.Server
			row.Live = true
			row.Source = "live"
			row.ServerStatus = live.ServerStatus
			row.ServerError = live.ServerError
		}
		if a, ok := usage[name]; ok {
			sort.Slice(a.durs, func(i, j int) bool { return a.durs[i] < a.durs[j] })
			var sum int64
			for _, d := range a.durs {
				sum += d
			}
			if len(a.durs) > 0 {
				row.AvgMs = sum / int64(len(a.durs))
			}
			row.Calls = a.calls
			row.Errors = a.errors
			if a.calls > 0 {
				row.SuccessPct = (a.calls - a.errors) * 100 / a.calls
			}
			row.P95ms = pctile(a.durs, 0.95)
			row.LastSeen = a.lastSeen.Format(time.RFC3339Nano)
			if row.Server == "" {
				row.Server = a.server
			}
			if row.Server == "" {
				row.Server = "native"
			}
			if row.ServerStatus == "" {
				if status, ok := liveStatuses[row.Server]; ok {
					row.ServerStatus = status.Status
					row.ServerError = status.Error
				}
			}
		}
		out = append(out, row)
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Live != out[j].Live {
			return out[i].Live
		}
		if out[i].Calls != out[j].Calls {
			return out[i].Calls > out[j].Calls
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (s *appServer) handleRegistryList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, registryCollectionResponse{Error: "method not allowed"})
		return
	}
	kind, err := registryKindFromString(r.URL.Query().Get("kind"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, registryCollectionResponse{Error: err.Error()})
		return
	}
	daemonClient, err := s.daemonClient()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, registryCollectionResponse{Error: err.Error()})
		return
	}
	rows, err := daemonClient.Registry().Search(r.Context(), kind, registry.Filter{
		Role:       strings.TrimSpace(r.URL.Query().Get("role")),
		Title:      strings.TrimSpace(r.URL.Query().Get("title")),
		Project:    strings.TrimSpace(r.URL.Query().Get("project")),
		Capability: strings.TrimSpace(r.URL.Query().Get("capability")),
		SkillName:  strings.TrimSpace(r.URL.Query().Get("skill_name")),
		Status:     strings.TrimSpace(r.URL.Query().Get("status")),
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, registryCollectionResponse{Error: err.Error()})
		return
	}
	if rows == nil {
		rows = []registry.Profile{}
	}
	writeJSON(w, http.StatusOK, registryCollectionResponse{Rows: rows})
}

func (s *appServer) handleRegistrySave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, actionResponse{Error: "method not allowed"})
		return
	}
	var req registrySaveRequest
	if err := decodeJSONBody(r, &req, false); err != nil {
		writeJSON(w, http.StatusBadRequest, actionResponse{Error: "invalid request body: " + err.Error()})
		return
	}
	req.DisplayName = strings.TrimSpace(req.DisplayName)
	req.Title = strings.TrimSpace(req.Title)
	req.Role = strings.TrimSpace(req.Role)
	req.Description = strings.TrimSpace(req.Description)
	req.Avatar = strings.TrimSpace(req.Avatar)
	req.Project = strings.TrimSpace(req.Project)
	req.Status = strings.TrimSpace(req.Status)
	req.HealthStatus = strings.TrimSpace(req.HealthStatus)
	req.HostAddress = strings.TrimSpace(req.HostAddress)
	req.LastUpdatedBy = strings.TrimSpace(req.LastUpdatedBy)
	req.Capabilities = normalizeRegistryStrings(req.Capabilities)
	if req.DisplayName == "" {
		writeJSON(w, http.StatusBadRequest, actionResponse{Error: "display_name required"})
		return
	}

	daemonClient, err := s.daemonClient()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, actionResponse{Error: err.Error()})
		return
	}

	if strings.TrimSpace(req.URN) == "" {
		if req.LastUpdatedBy == "" {
			req.LastUpdatedBy = "sysop-ui"
		}
		kind, err := registryKindFromString(req.Kind)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, actionResponse{Error: err.Error()})
			return
		}
		if req.Callback != nil {
			req.Callback.Scheme = strings.TrimSpace(req.Callback.Scheme)
			req.Callback.Target = strings.TrimSpace(req.Callback.Target)
			if req.Callback.Scheme == "" || req.Callback.Target == "" {
				writeJSON(w, http.StatusBadRequest, actionResponse{Error: "callback requires both scheme and target"})
				return
			}
		}
		_, err = daemonClient.Registry().Register(r.Context(), kind, registry.Profile{
			DisplayName:   req.DisplayName,
			Title:         req.Title,
			Role:          req.Role,
			Description:   req.Description,
			Avatar:        req.Avatar,
			Project:       req.Project,
			Status:        registry.Status(req.Status),
			Callback:      req.Callback,
			HealthStatus:  req.HealthStatus,
			HostAddress:   req.HostAddress,
			LastUpdatedBy: req.LastUpdatedBy,
			Capabilities:  req.Capabilities,
			Skills:        req.Skills,
			Links:         req.Links,
		})
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, actionResponse{Error: err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, actionResponse{Status: "saved"})
		return
	}

	if req.LastUpdatedBy == "" {
		writeJSON(w, http.StatusBadRequest, actionResponse{Error: "last_updated_by required for updates"})
		return
	}
	if req.Callback != nil {
		writeJSON(w, http.StatusBadRequest, actionResponse{Error: "callback edits are not supported by the registry patch API"})
		return
	}
	status := registry.Status(req.Status)
	_, err = daemonClient.Registry().UpdateSelf(r.Context(), strings.TrimSpace(req.URN), registry.UpdatePatch{
		DisplayName:   stringPtr(req.DisplayName),
		Title:         stringPtr(req.Title),
		Role:          stringPtr(req.Role),
		Description:   stringPtr(req.Description),
		Avatar:        stringPtr(req.Avatar),
		Project:       stringPtr(req.Project),
		Status:        &status,
		HealthStatus:  stringPtr(req.HealthStatus),
		HostAddress:   stringPtr(req.HostAddress),
		LastUpdatedBy: req.LastUpdatedBy,
		Capabilities: &registry.ArrayPatch[string]{
			Mode:  registry.ArrayModeReplace,
			Value: req.Capabilities,
		},
		Skills: &registry.ArrayPatch[registry.Skill]{
			Mode:  registry.ArrayModeReplace,
			Value: req.Skills,
		},
		Links: &registry.ArrayPatch[registry.Link]{
			Mode:  registry.ArrayModeReplace,
			Value: req.Links,
		},
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, actionResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, actionResponse{Status: "saved"})
}

func (s *appServer) handleRegistryDeregister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, actionResponse{Error: "method not allowed"})
		return
	}
	var req registryURNRequest
	if err := decodeJSONBody(r, &req, false); err != nil {
		writeJSON(w, http.StatusBadRequest, actionResponse{Error: "invalid request body: " + err.Error()})
		return
	}
	daemonClient, err := s.daemonClient()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, actionResponse{Error: err.Error()})
		return
	}
	if _, err := daemonClient.Registry().Deregister(r.Context(), strings.TrimSpace(req.URN)); err != nil {
		writeJSON(w, http.StatusInternalServerError, actionResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, actionResponse{Status: "deprecated"})
}

func (s *appServer) handleRegistrySync(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, actionResponse{Error: "method not allowed"})
		return
	}
	var req registryURNRequest
	if err := decodeJSONBody(r, &req, false); err != nil {
		writeJSON(w, http.StatusBadRequest, actionResponse{Error: "invalid request body: " + err.Error()})
		return
	}
	daemonClient, err := s.daemonClient()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, actionResponse{Error: err.Error()})
		return
	}
	_, synced, err := daemonClient.Registry().Sync(r.Context(), strings.TrimSpace(req.URN))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, actionResponse{Error: err.Error()})
		return
	}
	if !synced {
		writeJSON(w, http.StatusOK, actionResponse{Status: "noop"})
		return
	}
	writeJSON(w, http.StatusOK, actionResponse{Status: "synced"})
}

func (s *appServer) handleRegistryBootstrap(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, actionResponse{Error: "method not allowed"})
		return
	}
	var req registryBootstrapRequest
	if err := decodeJSONBody(r, &req, true); err != nil {
		writeJSON(w, http.StatusBadRequest, actionResponse{Error: "invalid request body: " + err.Error()})
		return
	}
	daemonClient, err := s.daemonClient()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, actionResponse{Error: err.Error()})
		return
	}
	writeBack := true
	if req.WriteBack != nil {
		writeBack = *req.WriteBack
	}
	report, err := daemonClient.Registry().Bootstrap(r.Context(), req.Force, req.Substrate, writeBack)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, actionResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// openStateDB loads the catalog and opens the Tether state DB. Returns
// errStateDBUnset when the catalog configures no state DB path.
func (s *appServer) openStateDB() (*store.Store, error) {
	cat, err := config.Load(s.catalogRoot)
	if err != nil {
		return nil, err
	}
	dbPath := config.Expand(cat.Global.Catalog.Defaults.StateDB)
	if dbPath == "" {
		return nil, errStateDBUnset
	}
	return store.Open(dbPath)
}

func (s *appServer) recordOperatorEvent(scope events.Scope, sessionID, kind string, payload any) {
	db, err := s.openStateDB()
	if err != nil {
		if !errors.Is(err, errStateDBUnset) {
			log.Printf("sysop: open state db for event %s: %v", kind, err)
		}
		return
	}
	defer db.Close()

	payloadJSON := ""
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			log.Printf("sysop: marshal payload for event %s: %v", kind, err)
			return
		}
		payloadJSON = string(b)
	}
	if _, _, err := db.InsertEvent(scope, sessionID, kind, payloadJSON); err != nil {
		log.Printf("sysop: persist event %s: %v", kind, err)
	}
}

// scopeOf classifies a recipient URN as "user", "agent", or "other".
func scopeOf(toURN string) string {
	addr, err := messaging.ParseURN(toURN)
	if err != nil {
		return "other"
	}
	switch addr.Kind {
	case messaging.KindUser:
		return "user"
	case messaging.KindAgent:
		return "agent"
	default:
		return "other"
	}
}

// payloadBody unwraps a message payload for display: a JSON-encoded string
// payload is unquoted to its text; anything else is returned verbatim.
func payloadBody(payload string) string {
	if payload == "" {
		return ""
	}
	var s string
	if err := json.Unmarshal([]byte(payload), &s); err == nil {
		return s
	}
	return payload
}

func groupToDTO(g registry.Profile, members []registry.GroupMember, msgs []registry.GroupMessage) groupDTO {
	memberDTOs := make([]groupMemberDTO, 0, len(members))
	for _, m := range members {
		memberDTOs = append(memberDTOs, groupMemberDTO{
			MemberURN:   m.MemberURN,
			DisplayName: m.DisplayName,
			Role:        string(m.Role),
			JoinedAt:    m.JoinedAt.Format(time.RFC3339Nano),
			LastReadSeq: m.LastReadSeq,
		})
	}
	sort.Slice(memberDTOs, func(i, j int) bool {
		ri := groupMemberRoleRank(memberDTOs[i].Role)
		rj := groupMemberRoleRank(memberDTOs[j].Role)
		if ri != rj {
			return ri < rj
		}
		if memberDTOs[i].DisplayName != memberDTOs[j].DisplayName {
			return memberDTOs[i].DisplayName < memberDTOs[j].DisplayName
		}
		return memberDTOs[i].MemberURN < memberDTOs[j].MemberURN
	})

	messageDTOs := make([]groupMessageDTO, 0, len(msgs))
	for _, msg := range msgs {
		messageDTOs = append(messageDTOs, groupMessageToDTO(msg))
	}

	return groupDTO{
		URN:         g.URN,
		DisplayName: g.DisplayName,
		Title:       g.Title,
		Description: g.Description,
		Status:      string(g.Status),
		CreatedAt:   g.CreatedAt.Format(time.RFC3339Nano),
		UpdatedAt:   g.UpdatedAt.Format(time.RFC3339Nano),
		Members:     memberDTOs,
		Messages:    messageDTOs,
		updatedAt:   g.UpdatedAt,
	}
}

func groupMemberRoleRank(role string) int {
	switch role {
	case "owner":
		return 0
	case "moderator":
		return 1
	case "member":
		return 2
	default:
		return 3
	}
}

func groupMessageToDTO(m registry.GroupMessage) groupMessageDTO {
	subject, body := groupPayloadText(m.Payload)
	return groupMessageDTO{
		ID:          m.ID,
		GroupURN:    m.GroupURN,
		GroupSeq:    m.GroupSeq,
		FromURN:     m.FromURN,
		Kind:        m.Kind,
		ThreadID:    m.ThreadID,
		Subject:     subject,
		Body:        body,
		Payload:     string(m.Payload),
		ContentType: m.ContentType,
		CreatedAt:   m.CreatedAt.Format(time.RFC3339Nano),
	}
}

func groupPayloadText(payload json.RawMessage) (subject, body string) {
	trimmed := strings.TrimSpace(string(payload))
	if trimmed == "" {
		return "", ""
	}
	switch trimmed[0] {
	case '{':
		var obj map[string]json.RawMessage
		if err := json.Unmarshal([]byte(trimmed), &obj); err != nil {
			return "", trimmed
		}
		return firstPayloadString(obj, "subject", "title"),
			firstPayloadString(obj, "body", "summary", "text", "message")
	case '"':
		var s string
		if err := json.Unmarshal([]byte(trimmed), &s); err == nil {
			return "", s
		}
		return "", trimmed
	default:
		return "", trimmed
	}
}

func firstPayloadString(obj map[string]json.RawMessage, keys ...string) string {
	for _, k := range keys {
		raw, ok := obj[k]
		if !ok {
			continue
		}
		var s string
		if err := json.Unmarshal(raw, &s); err == nil && strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

func nullableInt(valid bool, value int) *int {
	if !valid {
		return nil
	}
	return &value
}

func nullableString(valid bool, value string) string {
	if !valid {
		return ""
	}
	return value
}

func prettyJSON(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return ""
	}
	return string(b)
}

func prettyYAML(v any) (string, error) {
	b, err := yaml.Marshal(v)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func unifiedTextDiff(current, next string) string {
	a := splitDiffLines(current)
	b := splitDiffLines(next)
	if len(a) == 0 && len(b) == 0 {
		return ""
	}

	lcs := make([][]int, len(a)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}

	lines := make([]string, 0, len(a)+len(b))
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		if a[i] == b[j] {
			lines = append(lines, " "+a[i])
			i++
			j++
			continue
		}
		if lcs[i+1][j] >= lcs[i][j+1] {
			lines = append(lines, "-"+a[i])
			i++
			continue
		}
		lines = append(lines, "+"+b[j])
		j++
	}
	for i < len(a) {
		lines = append(lines, "-"+a[i])
		i++
	}
	for j < len(b) {
		lines = append(lines, "+"+b[j])
		j++
	}
	return strings.Join(lines, "\n")
}

func splitDiffLines(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	return strings.Split(raw, "\n")
}

func (s *appServer) settingsServer() settingsServerDTO {
	return settingsServerDTO{
		HTTPAddr:    s.addr,
		CatalogRoot: s.catalogRoot,
		PID:         os.Getpid(),
		StartedAt:   s.startedAt.Format(time.RFC3339Nano),
		UptimeSec:   int64(time.Since(s.startedAt).Seconds()),
	}
}

func aiUsageBudgetFromConfig(in config.AIUsageBudgetPolicyConfig) aiUsageBudgetDTO {
	return aiUsageBudgetDTO{
		MaxCostUSD: in.MaxCostUSD,
		Window:     in.Window,
		Scope:      in.Scope,
	}
}

func aiCatalogVendorProviderID(providerType string) (string, bool) {
	switch strings.TrimSpace(providerType) {
	case "anthropic":
		return "anthropic", true
	case "openai", "openai-compatible":
		return "openai", true
	case "gemini":
		return "google", true
	default:
		return "", false
	}
}

func aiCatalogModelFromRef(ref modelsdev.ModelRef) aiCatalogModelDTO {
	return aiCatalogModelDTO{
		ID:                  ref.ID,
		Name:                ref.Name,
		Family:              ref.Family,
		ContextWindow:       ref.Limit.ContextWindow,
		MaxOutputTokens:     ref.Limit.MaxOutputTokens,
		InputModalities:     append([]string(nil), ref.Modality.Input...),
		OutputModalities:    append([]string(nil), ref.Modality.Output...),
		SupportsTools:       ref.Capabilities.ToolCall,
		SupportsReasoning:   ref.Capabilities.Reasoning,
		SupportsAttachments: ref.Capabilities.Attachment,
		InputCostUSD:        ref.Cost.Input,
		OutputCostUSD:       ref.Cost.Output,
	}
}

func aiPolicyFromConfig(in config.AIPolicyConfig) aiPolicyDTO {
	return aiPolicyDTO{
		AllowReasoning:   in.AllowReasoning,
		AllowTools:       in.AllowTools,
		AllowAttachments: in.AllowAttachments,
		MaxOutputTokens:  in.MaxOutputTokens,
		MaxCostUSD:       in.MaxCostUSD,
		UsageBudget:      aiUsageBudgetFromConfig(in.UsageBudget),
	}
}

func aiConfigFromConfig(in config.AIConfig) aiConfigDTO {
	out := aiConfigDTO{
		Policy:               aiPolicyFromConfig(in.Policy),
		DefaultProviderOrder: append([]string(nil), in.Routing.DefaultProviderOrder...),
		Providers:            make([]aiProviderDTO, 0, len(in.Providers)),
		Routes:               make([]aiRouteDTO, 0, len(in.Routing.Routes)),
	}
	for _, provider := range in.Providers {
		out.Providers = append(out.Providers, aiProviderDTO{
			ID:           provider.ID,
			Type:         provider.Type,
			Model:        provider.Model,
			Models:       append([]string(nil), provider.Models...),
			DefaultModel: provider.DefaultModel,
			SecretRef:    provider.SecretRef,
			BaseURL:      provider.BaseURL,
			Enabled:      provider.Enabled,
			Policy:       aiPolicyFromConfig(provider.Policy),
		})
	}
	for _, route := range in.Routing.Routes {
		out.Routes = append(out.Routes, aiRouteDTO{
			Provider:          route.Provider,
			Model:             route.Model,
			Mode:              route.Mode,
			Intent:            route.Intent,
			RequiresReasoning: route.RequiresReasoning,
			RequiresTools:     route.RequiresTools,
			Policy: aiPolicyDTO{
				AllowReasoning:   route.AllowReasoning,
				AllowTools:       route.AllowTools,
				AllowAttachments: route.AllowAttachments,
				MaxOutputTokens:  route.MaxOutputTokens,
				MaxCostUSD:       route.MaxCostUSD,
				UsageBudget:      aiUsageBudgetFromConfig(route.UsageBudget),
			},
		})
	}
	return out
}

func configUsageBudgetFromDTO(in aiUsageBudgetDTO) config.AIUsageBudgetPolicyConfig {
	return config.AIUsageBudgetPolicyConfig{
		MaxCostUSD: in.MaxCostUSD,
		Window:     strings.TrimSpace(in.Window),
		Scope:      strings.TrimSpace(in.Scope),
	}
}

func configPolicyFromDTO(in aiPolicyDTO) config.AIPolicyConfig {
	return config.AIPolicyConfig{
		AllowReasoning:   in.AllowReasoning,
		AllowTools:       in.AllowTools,
		AllowAttachments: in.AllowAttachments,
		MaxOutputTokens:  in.MaxOutputTokens,
		MaxCostUSD:       in.MaxCostUSD,
		UsageBudget:      configUsageBudgetFromDTO(in.UsageBudget),
	}
}

func configAIFromDTO(in aiConfigDTO) (config.AIConfig, error) {
	out := config.AIConfig{
		Policy: configPolicyFromDTO(in.Policy),
		Routing: config.AIRoutingConfig{
			DefaultProviderOrder: cleanStringList(in.DefaultProviderOrder),
			Routes:               make([]config.AIRouteConfig, 0, len(in.Routes)),
		},
		Providers: make([]config.AIProviderConfig, 0, len(in.Providers)),
	}
	for _, provider := range in.Providers {
		provider.ID = strings.TrimSpace(provider.ID)
		provider.Type = strings.TrimSpace(provider.Type)
		provider.Model = strings.TrimSpace(provider.Model)
		provider.DefaultModel = strings.TrimSpace(provider.DefaultModel)
		provider.SecretRef = strings.TrimSpace(provider.SecretRef)
		provider.BaseURL = strings.TrimSpace(provider.BaseURL)
		if provider.ID == "" {
			return config.AIConfig{}, fmt.Errorf("provider id is required")
		}
		out.Providers = append(out.Providers, config.AIProviderConfig{
			ID:           provider.ID,
			Type:         provider.Type,
			Model:        provider.Model,
			Models:       cleanStringList(provider.Models),
			DefaultModel: provider.DefaultModel,
			SecretRef:    provider.SecretRef,
			BaseURL:      provider.BaseURL,
			Enabled:      provider.Enabled,
			Policy:       configPolicyFromDTO(provider.Policy),
		})
	}
	for _, route := range in.Routes {
		route.Provider = strings.TrimSpace(route.Provider)
		route.Model = strings.TrimSpace(route.Model)
		out.Routing.Routes = append(out.Routing.Routes, config.AIRouteConfig{
			Provider:          route.Provider,
			Model:             route.Model,
			Mode:              strings.TrimSpace(route.Mode),
			Intent:            strings.TrimSpace(route.Intent),
			RequiresReasoning: route.RequiresReasoning,
			RequiresTools:     route.RequiresTools,
			AllowReasoning:    route.Policy.AllowReasoning,
			AllowTools:        route.Policy.AllowTools,
			AllowAttachments:  route.Policy.AllowAttachments,
			MaxOutputTokens:   route.Policy.MaxOutputTokens,
			MaxCostUSD:        route.Policy.MaxCostUSD,
			UsageBudget:       configUsageBudgetFromDTO(route.Policy.UsageBudget),
		})
	}
	return out, nil
}

func (s *appServer) daemonClient() (*muxclient.Client, error) {
	cat, err := config.Load(s.catalogRoot)
	if err != nil {
		return nil, err
	}
	listenAddr := cat.Global.Daemon.ListenAddr
	if listenAddr == "" {
		listenAddr = "unix:~/.tether/run/muxd.sock"
	}
	if strings.HasPrefix(listenAddr, "unix:") {
		listenAddr = "unix:" + config.Expand(strings.TrimPrefix(listenAddr, "unix:"))
	}
	return muxclient.New(listenAddr), nil
}

func registryKindFromString(raw string) (registry.Kind, error) {
	switch strings.TrimSpace(raw) {
	case "agent":
		return registry.KindAgent, nil
	case "project":
		return registry.KindProject, nil
	default:
		return "", fmt.Errorf("kind must be agent or project")
	}
}

func stringPtr(v string) *string {
	return &v
}

func normalizeRegistryStrings(items []string) []string {
	if len(items) == 0 {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		trimmed := strings.TrimSpace(item)
		if trimmed == "" {
			continue
		}
		out = append(out, trimmed)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

var catalogIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func validCatalogID(id string) bool {
	return catalogIDRE.MatchString(id)
}

func validateMCPServerSave(req mcpServerSaveRequest) error {
	if !validCatalogID(req.ID) {
		return fmt.Errorf("invalid id")
	}
	switch req.Transport {
	case "stdio":
		if strings.TrimSpace(req.Command) == "" {
			return fmt.Errorf("command required for stdio servers")
		}
	case "sse":
		if strings.TrimSpace(req.URL) == "" {
			return fmt.Errorf("url required for sse servers")
		}
	default:
		return fmt.Errorf("transport must be stdio or sse")
	}
	return nil
}

func mergeMCPEnv(existing map[string]string, incoming map[string]string) map[string]string {
	if len(incoming) == 0 {
		return nil
	}
	out := map[string]string{}
	for key, value := range incoming {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		value = strings.TrimSpace(value)
		if value == secretPreserveMarker {
			if existingValue, ok := existing[key]; ok {
				out[key] = existingValue
			}
			continue
		}
		out[key] = value
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (s *appServer) providerFromSaveRequest(req providerSaveRequest) (config.Provider, string, error) {
	req.ID = strings.TrimSpace(req.ID)
	req.Type = strings.TrimSpace(req.Type)
	req.Provider = strings.TrimSpace(req.Provider)
	req.RuntimeKind = strings.TrimSpace(req.RuntimeKind)
	req.Command = strings.TrimSpace(req.Command)
	req.Adapter = strings.TrimSpace(req.Adapter)
	req.BootstrapMode = strings.TrimSpace(req.BootstrapMode)
	req.BootstrapPrefix = strings.TrimSpace(req.BootstrapPrefix)
	req.EnvMode = strings.TrimSpace(req.EnvMode)
	if req.Type == "" {
		req.Type = "cli"
	}
	if req.EnvMode == "" {
		req.EnvMode = "merge"
	}
	if !validCatalogID(req.ID) {
		return config.Provider{}, "", fmt.Errorf("invalid id")
	}
	cat, err := config.Load(s.catalogRoot)
	if err != nil {
		return config.Provider{}, "", err
	}
	provider, path, found, err := s.rawProvider(req.ID)
	if err != nil {
		return config.Provider{}, "", err
	}
	if !found {
		path = filepath.Join(catalogSubdir(s.catalogRoot, cat.Global.Catalog.Roots.Providers, "providers"), req.ID+".yaml")
		provider = config.Provider{ID: req.ID}
	}
	provider.ID = req.ID
	provider.Type = req.Type
	provider.Provider = req.Provider
	provider.RuntimeKind = req.RuntimeKind
	provider.Command = req.Command
	provider.Args = cleanStringList(req.Args)
	provider.Adapter = req.Adapter
	provider.Bootstrap.Mode = req.BootstrapMode
	provider.Bootstrap.PromptPrefix = req.BootstrapPrefix
	provider.Env.Mode = req.EnvMode
	provider.Env.Passthrough = cleanStringList(req.EnvPassthrough)
	provider.Env.Redact = cleanStringList(req.EnvRedact)

	check := *cat
	check.Providers = make(map[string]config.Provider, len(cat.Providers)+1)
	for id, existing := range cat.Providers {
		check.Providers[id] = existing
	}
	check.Providers[provider.ID] = provider
	if err := check.Validate(); err != nil {
		return config.Provider{}, "", err
	}
	return provider, path, nil
}

func (s *appServer) rawProvider(id string) (config.Provider, string, bool, error) {
	cat, err := config.Load(s.catalogRoot)
	if err != nil {
		return config.Provider{}, "", false, err
	}
	dir := catalogSubdir(s.catalogRoot, cat.Global.Catalog.Roots.Providers, "providers")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return config.Provider{}, "", false, nil
		}
		return config.Provider{}, "", false, fmt.Errorf("read providers dir: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(entry.Name()))
		if ext != ".yaml" && ext != ".yml" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		b, err := os.ReadFile(path) //nolint:gosec // G304: catalog-controlled provider file
		if err != nil {
			return config.Provider{}, "", false, err
		}
		var raw config.Provider
		if err := yaml.Unmarshal(b, &raw); err != nil {
			return config.Provider{}, "", false, fmt.Errorf("parse %s: %w", entry.Name(), err)
		}
		if raw.ID == id {
			return raw, path, true, nil
		}
	}
	return config.Provider{}, "", false, nil
}

func providerLaunchRefs(cat *config.Catalog) map[string]int {
	refs := map[string]int{}
	if cat == nil {
		return refs
	}
	for _, launch := range cat.Launches {
		if launch.Provider != "" {
			refs[launch.Provider]++
		}
	}
	return refs
}

type launchPreviewChange struct {
	Launch             config.Launch
	Path               string
	Found              bool
	CurrentProfile     string
	NextProfile        string
	CurrentPlan        string
	NextPlan           string
	CurrentProfileYAML string
	NextProfileYAML    string
	ProfileDiff        string
	PlanDiff           string
	ProfileChanged     bool
	PlanChanged        bool
	WillCreateBackup   bool
	CommentLossRisk    bool
	CurrentPlanError   string
	NextPlanError      string
	Warnings           []string
}

func (s *appServer) previewLaunchChange(req launchSaveRequest) (launchPreviewChange, error) {
	req.ID = strings.TrimSpace(req.ID)
	req.Project = strings.TrimSpace(req.Project)
	req.Agent = strings.TrimSpace(req.Agent)
	req.Provider = strings.TrimSpace(req.Provider)
	req.WorkspaceMode = strings.TrimSpace(req.WorkspaceMode)
	if req.WorkspaceMode == "" {
		req.WorkspaceMode = "default"
	}
	if !validCatalogID(req.ID) {
		return launchPreviewChange{}, fmt.Errorf("invalid id")
	}
	cat, err := config.Load(s.catalogRoot)
	if err != nil {
		return launchPreviewChange{}, err
	}
	if _, ok := cat.Projects[req.Project]; !ok {
		return launchPreviewChange{}, fmt.Errorf("unknown project %q", req.Project)
	}
	if _, ok := cat.Agents[req.Agent]; !ok {
		return launchPreviewChange{}, fmt.Errorf("unknown agent %q", req.Agent)
	}
	if _, ok := cat.Providers[req.Provider]; !ok {
		return launchPreviewChange{}, fmt.Errorf("unknown provider %q", req.Provider)
	}
	launch, path, found, err := s.rawLaunch(req.ID)
	if err != nil {
		return launchPreviewChange{}, err
	}
	currentProfileYAML := ""
	if !found {
		path = filepath.Join(catalogSubdir(s.catalogRoot, cat.Global.Catalog.Roots.Launches, "launches"), req.ID+".yaml")
		launch = config.Launch{ID: req.ID}
	} else {
		b, err := os.ReadFile(path) //nolint:gosec // G304: catalog-controlled launch file
		if err != nil {
			return launchPreviewChange{}, err
		}
		currentProfileYAML = strings.TrimSpace(string(b))
	}
	launch.ID = req.ID
	launch.Project = req.Project
	launch.Agent = req.Agent
	launch.Provider = req.Provider
	launch.Workspace.Mode = req.WorkspaceMode
	launch.Workspace.WorktreeName = strings.TrimSpace(req.WorktreeName)
	launch.Prompt.IncludeProjectBoot = req.IncludeProjectBoot
	launch.Prompt.IncludeAgentBoot = req.IncludeAgentBoot
	launch.Prompt.IncludeKnowledgeBase = req.IncludeKnowledgeBase
	launch.MCP.Servers = cleanStringList(req.MCPServers)
	launch.Overrides.Env = cleanStringMap(req.EnvOverrides)
	launch.Injection.NativeFiles = cleanInjectedFiles(req.NativeFiles)
	launch.Injection.BootDirOverlay = cleanInjectedFiles(req.BootDirOverlay)

	check := *cat
	check.Launches = make(map[string]config.Launch, len(cat.Launches)+1)
	for id, existing := range cat.Launches {
		check.Launches[id] = existing
	}
	check.Launches[launch.ID] = launch
	if err := check.Validate(); err != nil {
		return launchPreviewChange{}, err
	}

	nextPlan, err := launchplan.Resolve(&check, launchplan.Input{
		LaunchID:    launch.ID,
		CatalogRoot: s.catalogRoot,
	})
	if err != nil {
		return launchPreviewChange{}, err
	}
	nextProfileYAML, err := prettyYAML(launch)
	if err != nil {
		return launchPreviewChange{}, err
	}
	nextPlanJSON := prettyJSON(nextPlan)

	change := launchPreviewChange{
		Launch:             launch,
		Path:               path,
		Found:              found,
		NextProfile:        prettyJSON(launch),
		NextPlan:           nextPlanJSON,
		CurrentProfileYAML: currentProfileYAML,
		NextProfileYAML:    nextProfileYAML,
		ProfileDiff:        unifiedTextDiff(currentProfileYAML, nextProfileYAML),
		PlanDiff:           unifiedTextDiff("", nextPlanJSON),
		ProfileChanged:     strings.TrimSpace(currentProfileYAML) != strings.TrimSpace(nextProfileYAML),
		PlanChanged:        true,
		WillCreateBackup:   found,
		CommentLossRisk:    found,
		Warnings:           launchPreviewWarnings(launch, found),
	}
	if found {
		change.CurrentProfile = prettyJSON(cat.Launches[launch.ID])
		if currentPlan, err := launchplan.Resolve(cat, launchplan.Input{
			LaunchID:    launch.ID,
			CatalogRoot: s.catalogRoot,
		}); err != nil {
			change.CurrentPlanError = err.Error()
		} else {
			change.CurrentPlan = prettyJSON(currentPlan)
			change.PlanDiff = unifiedTextDiff(change.CurrentPlan, change.NextPlan)
			change.PlanChanged = strings.TrimSpace(change.CurrentPlan) != strings.TrimSpace(change.NextPlan)
		}
		if strings.Contains(currentProfileYAML, "#") {
			change.CommentLossRisk = true
		}
	}
	return change, nil
}

func (s *appServer) rawLaunch(id string) (config.Launch, string, bool, error) {
	cat, err := config.Load(s.catalogRoot)
	if err != nil {
		return config.Launch{}, "", false, err
	}
	dir := catalogSubdir(s.catalogRoot, cat.Global.Catalog.Roots.Launches, "launches")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return config.Launch{}, "", false, nil
		}
		return config.Launch{}, "", false, fmt.Errorf("read launches dir: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(entry.Name()))
		if ext != ".yaml" && ext != ".yml" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		b, err := os.ReadFile(path) //nolint:gosec // G304: catalog-controlled launch file
		if err != nil {
			return config.Launch{}, "", false, err
		}
		var raw config.Launch
		if err := yaml.Unmarshal(b, &raw); err != nil {
			return config.Launch{}, "", false, fmt.Errorf("parse %s: %w", entry.Name(), err)
		}
		if raw.ID == id {
			return raw, path, true, nil
		}
	}
	return config.Launch{}, "", false, nil
}

func cleanStringList(items []string) []string {
	out := make([]string, 0, len(items))
	seen := map[string]struct{}{}
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if _, ok := seen[item]; ok {
			continue
		}
		seen[item] = struct{}{}
		out = append(out, item)
	}
	return out
}

func cleanStringMap(items map[string]string) map[string]string {
	if len(items) == 0 {
		return nil
	}
	out := map[string]string{}
	for key, value := range items {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		out[key] = value
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func cleanInjectedFiles(items []injectedFileRequest) []config.InjectedFile {
	out := make([]config.InjectedFile, 0, len(items))
	for _, item := range items {
		kind := strings.TrimSpace(item.Kind)
		id := strings.TrimSpace(item.ID)
		relPath := strings.TrimSpace(item.RelPath)
		content := item.Content
		source := strings.TrimSpace(item.Source)
		mode := item.Mode
		if kind == "" && id == "" && relPath == "" && content == "" && source == "" && mode == 0 {
			continue
		}
		out = append(out, config.InjectedFile{
			Kind:    kind,
			ID:      id,
			RelPath: relPath,
			Content: content,
			Source:  source,
			Mode:    mode,
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func launchPreviewWarnings(launch config.Launch, existing bool) []string {
	var warnings []string
	if existing {
		warnings = append(warnings, "Saving rewrites the launch YAML from structured data. Existing comments and hand formatting will not be preserved.")
		warnings = append(warnings, "A timestamped backup will be written beside the launch YAML before overwrite.")
	} else {
		warnings = append(warnings, "Saving creates a new launch YAML file at the target path shown in preview.")
	}
	if len(launch.Overrides.Env) > 0 {
		warnings = append(warnings, "Launch env overrides are persisted in catalog YAML. Do not place secrets here.")
	}
	if len(launch.Injection.NativeFiles) > 0 || len(launch.Injection.BootDirOverlay) > 0 {
		warnings = append(warnings, "Injected file content is persisted at rest via launch plans. Use provider env passthrough for secrets.")
	}
	return warnings
}

func (s *appServer) rawMCPServer(id string) (config.MCPServerEntry, string, bool, error) {
	dir := filepath.Join(s.catalogRoot, "mcp-servers")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return config.MCPServerEntry{}, "", false, nil
		}
		return config.MCPServerEntry{}, "", false, fmt.Errorf("read mcp-servers dir: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(entry.Name()))
		if ext != ".yaml" && ext != ".yml" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		b, err := os.ReadFile(path) //nolint:gosec // G304: catalog-controlled MCP server file
		if err != nil {
			return config.MCPServerEntry{}, "", false, err
		}
		var raw config.MCPServerEntry
		if err := yaml.Unmarshal(b, &raw); err != nil {
			return config.MCPServerEntry{}, "", false, fmt.Errorf("parse %s: %w", entry.Name(), err)
		}
		if raw.ID == id {
			return raw, path, true, nil
		}
	}
	return config.MCPServerEntry{}, "", false, nil
}

func settingsDaemon(cat *config.Catalog) settingsDaemonDTO {
	listenAddr := ""
	pidFile := ""
	shutdownTimeout := ""
	permissionMode := config.PermissionModeDefault
	launchEngine := ""
	launchSpecsRoot := effectiveLaunchSpecsRoot(cat)
	if cat != nil {
		listenAddr = cat.Global.Daemon.ListenAddr
		pidFile = config.Expand(cat.Global.Daemon.PIDFile)
		shutdownTimeout = cat.Global.Daemon.ShutdownTimeout
		permissionMode = config.EffectivePermissionMode(cat.Global, config.Agent{})
		launchEngine = cat.Global.Catalog.Defaults.LaunchEngine
	}
	kind, endpoint := splitListenAddr(listenAddr)
	pid, pidRunning := readPIDStatus(pidFile)
	return settingsDaemonDTO{
		ListenAddr:      listenAddr,
		ListenKind:      kind,
		ListenEndpoint:  endpoint,
		SocketExists:    kind == "unix" && pathExists(endpoint),
		PIDFile:         pidFile,
		PIDFileExists:   pathExists(pidFile),
		PID:             pid,
		PIDRunning:      pidRunning,
		ShutdownTimeout: shutdownTimeout,
		PermissionMode:  permissionMode,
		LaunchEngine:    launchEngine,
		LaunchSpecsRoot: launchSpecsRoot,
	}
}

func splitListenAddr(addr string) (kind, endpoint string) {
	kind, endpoint, ok := strings.Cut(addr, ":")
	if !ok {
		return "", addr
	}
	if kind == "unix" {
		endpoint = config.Expand(endpoint)
	}
	return kind, endpoint
}

func readPIDStatus(path string) (int, bool) {
	if path == "" {
		return 0, false
	}
	b, err := os.ReadFile(path) //nolint:gosec // G304: catalog-configured local PID file
	if err != nil {
		return 0, false
	}
	pidText := strings.TrimSpace(string(b))
	if pidText == "" {
		return 0, false
	}
	var pid int
	if _, err := fmt.Sscanf(pidText, "%d", &pid); err != nil || pid <= 0 {
		return 0, false
	}
	return pid, processRunning(pid)
}

func processRunning(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

func catalogSubdir(catalogRoot, configured, fallback string) string {
	if configured == "" {
		configured = fallback
	}
	if filepath.IsAbs(configured) || strings.HasPrefix(configured, "~") {
		return config.Expand(configured)
	}
	return filepath.Join(catalogRoot, configured)
}

func effectiveLaunchSpecsRoot(cat *config.Catalog) string {
	if v := os.Getenv("TETHER_LAUNCH_SPECS_ROOT"); v != "" {
		return config.Expand(v)
	}
	if cat != nil {
		if v := cat.Global.Catalog.Defaults.LaunchSpecsRoot; v != "" {
			return config.Expand(v)
		}
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".tether", "launch-specs")
}

func pathDTO(path string) settingsPathDTO {
	return settingsPathDTO{Path: path, Exists: pathExists(path)}
}

func pathExists(path string) bool {
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

func fileModTime(path string) time.Time {
	if path == "" {
		return time.Time{}
	}
	info, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return info.ModTime()
}

func latestYAMLModTime(dir string) time.Time {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return time.Time{}
	}
	var latest time.Time
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(entry.Name()))
		if ext != ".yaml" && ext != ".yml" {
			continue
		}
		if info, err := entry.Info(); err == nil && info.ModTime().After(latest) {
			latest = info.ModTime()
		}
	}
	return latest
}

func maxTime(values ...time.Time) time.Time {
	var latest time.Time
	for _, value := range values {
		if value.After(latest) {
			latest = value
		}
	}
	return latest
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("write response: %v", err)
	}
}

func decodeJSONBody(r *http.Request, out any, allowEmpty bool) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		if allowEmpty && errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	return errors.New("request body must contain a single JSON value")
}

func writeDaemonActionError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	if errors.Is(err, muxclient.ErrDaemonUnreachable) {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, actionResponse{Error: err.Error()})
}

func cleanupCandidateSessionIDs(db *sql.DB, cutoff string, limit int) ([]string, error) {
	rows, err := db.Query(
		`SELECT id FROM sessions
		  WHERE ended_at IS NOT NULL
		    AND ended_at != ''
		    AND ended_at < ?
		    AND state IN ('completed', 'failed', 'killed')
		  ORDER BY ended_at ASC
		  LIMIT ?`,
		cutoff, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("query cleanup candidates: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func deleteSessionsByID(db *sql.DB, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	placeholders := strings.TrimRight(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	for _, table := range []string{"client_attachments", "events", "launch_plans", "proxy_events"} {
		if _, err := tx.Exec(`DELETE FROM `+table+` WHERE session_id IN (`+placeholders+`)`, args...); err != nil {
			return fmt.Errorf("delete %s: %w", table, err)
		}
	}
	if _, err := tx.Exec(`DELETE FROM sessions WHERE id IN (`+placeholders+`)`, args...); err != nil {
		return fmt.Errorf("delete sessions: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	committed = true
	return nil
}

func readYAMLFile(path string, out any) error {
	b, err := os.ReadFile(path) //nolint:gosec // G304: catalog file path
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if err := yaml.Unmarshal(b, out); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	return nil
}

func writeYAMLFile(path string, value any, mode os.FileMode) error {
	b, err := yaml.Marshal(value)
	if err != nil {
		return fmt.Errorf("marshal %s: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp for %s: %w", path, err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp %s: %w", tmpPath, err)
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}

func writeCatalogYAMLFile(path string, value any, mode os.FileMode) (string, error) {
	var backupPath string
	if pathExists(path) {
		var err error
		backupPath, err = backupCatalogFile(path)
		if err != nil {
			return "", err
		}
	}
	if err := writeYAMLFile(path, value, mode); err != nil {
		return "", err
	}
	return backupPath, nil
}

func removeCatalogFileWithBackup(path string) (string, error) {
	backupPath, err := backupCatalogFile(path)
	if err != nil {
		return "", err
	}
	if err := os.Remove(path); err != nil {
		return "", fmt.Errorf("remove %s: %w", path, err)
	}
	return backupPath, nil
}

func backupCatalogFile(path string) (string, error) {
	if !pathExists(path) {
		return "", nil
	}
	src, err := os.ReadFile(path) //nolint:gosec // G304: operator-owned catalog path
	if err != nil {
		return "", fmt.Errorf("read %s for backup: %w", path, err)
	}
	backupPath := uniqueBackupPath(path)
	if err := os.WriteFile(backupPath, src, 0o600); err != nil {
		return "", fmt.Errorf("write backup %s: %w", backupPath, err)
	}
	return backupPath, nil
}

func uniqueBackupPath(path string) string {
	stamp := time.Now().UTC().Format("20060102T150405.000000000Z")
	base := fmt.Sprintf("%s.bak-%s", path, stamp)
	if !pathExists(base) {
		return base
	}
	for i := 1; ; i++ {
		candidate := fmt.Sprintf("%s.%d", base, i)
		if !pathExists(candidate) {
			return candidate
		}
	}
}

func allowedSystemResource(resource string) bool {
	switch resource {
	case "tether-dev", "tether-daemon-service":
		return true
	default:
		return false
	}
}

func allowedSystemResourceAction(action string) bool {
	switch action {
	case "status", "reload", "apply", "deploy":
		return true
	default:
		return false
	}
}

func runCerberusResourceAction(ctx context.Context, resource, action string) (string, error) {
	if output, err := runCerberusSocketResourceAction(ctx, resource, action); err == nil {
		return output, nil
	} else if shouldRetryCerberusTetherRegister(resource, err) {
		if healErr := ensureCerberusTetherRegistrationCompat(ctx); healErr == nil {
			if retryOutput, retryErr := runCerberusSocketResourceAction(ctx, resource, action); retryErr == nil {
				return retryOutput, nil
			}
		}
	}
	runCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(runCtx, "cerberus", "resource", action, resource)
	out, err := cmd.CombinedOutput()
	output := strings.TrimSpace(string(out))
	if runCtx.Err() != nil {
		return output, runCtx.Err()
	}
	if err != nil {
		return output, fmt.Errorf("cerberus resource %s %s: %w", action, resource, err)
	}
	return output, nil
}

type cerberusErrorResponse struct {
	Success bool   `json:"success"`
	Error   string `json:"error"`
}

type cerberusRegistryFile struct {
	Entries []struct {
		Owner string `yaml:"owner"`
		Path  string `yaml:"path"`
	} `yaml:"entries"`
}

func runCerberusSocketResourceAction(ctx context.Context, resource, action string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve cerberus socket: %w", err)
	}
	socketPath := filepath.Join(home, ".cerberus", "cerberus.sock")
	if !pathExists(socketPath) {
		return "", fmt.Errorf("cerberus socket not found at %s", socketPath)
	}
	method := http.MethodPost
	path := "/resources/" + url.PathEscape(resource) + "/" + action
	if action == "status" {
		method = http.MethodGet
	}
	runCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", socketPath)
		},
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	req, err := http.NewRequestWithContext(runCtx, method, "http://cerberus-daemon"+path, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	output := strings.TrimSpace(string(body))
	if method == http.MethodPost {
		var apiErr cerberusErrorResponse
		if err := json.Unmarshal(body, &apiErr); err == nil && !apiErr.Success && strings.TrimSpace(apiErr.Error) != "" {
			return output, errors.New(strings.TrimSpace(apiErr.Error))
		}
	}
	if resp.StatusCode >= 400 {
		var apiErr cerberusErrorResponse
		if err := json.Unmarshal(body, &apiErr); err == nil && strings.TrimSpace(apiErr.Error) != "" {
			return output, errors.New(strings.TrimSpace(apiErr.Error))
		}
		return output, fmt.Errorf("cerberus socket %s %s: http %d", method, path, resp.StatusCode)
	}
	return output, nil
}

func shouldRetryCerberusTetherRegister(resource string, err error) bool {
	if err == nil || !strings.HasPrefix(resource, "tether-") {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "not found")
}

func ensureCerberusTetherRegistrationCompat(ctx context.Context) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	registryPath := filepath.Join(home, ".cerberus", "registry.yaml")
	data, err := os.ReadFile(registryPath)
	if err != nil {
		return err
	}
	var reg cerberusRegistryFile
	if err := yaml.Unmarshal(data, &reg); err != nil {
		return err
	}
	projectPath := ""
	for _, entry := range reg.Entries {
		if entry.Owner == "tether" && strings.TrimSpace(entry.Path) != "" {
			projectPath = strings.TrimSpace(entry.Path)
			break
		}
	}
	if projectPath == "" {
		return fmt.Errorf("tether project not found in cerberus registry")
	}
	src, err := os.ReadFile(projectPath)
	if err != nil {
		return err
	}
	sanitized := stripRegistryURN(src)
	tmpFile, err := os.CreateTemp("", "tether-cerberus-compat-*.yaml")
	if err != nil {
		return err
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)
	if _, err := tmpFile.Write(sanitized); err != nil {
		tmpFile.Close()
		return err
	}
	if err := tmpFile.Close(); err != nil {
		return err
	}
	runCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(runCtx, "cerberus", "register", tmpPath)
	out, err := cmd.CombinedOutput()
	if runCtx.Err() != nil {
		return runCtx.Err()
	}
	if err != nil {
		return fmt.Errorf("cerberus register sanitized tether config: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

func stripRegistryURN(src []byte) []byte {
	lines := bytes.Split(src, []byte{'\n'})
	out := make([][]byte, 0, len(lines))
	for _, line := range lines {
		trimmed := bytes.TrimSpace(line)
		if bytes.HasPrefix(trimmed, []byte("registry_urn:")) {
			continue
		}
		out = append(out, line)
	}
	return bytes.Join(out, []byte{'\n'})
}

type flushResponseWriter struct {
	w io.Writer
	f http.Flusher
}

func (fw flushResponseWriter) Write(p []byte) (int, error) {
	n, err := fw.w.Write(p)
	fw.f.Flush()
	return n, err
}
