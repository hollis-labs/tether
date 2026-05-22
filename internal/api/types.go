package api

import (
	"context"
	"io"

	"github.com/hollis-labs/go-agent-sessions/agentsessions"

	"github.com/hollis-labs/tether/internal/store"
)

// LaunchService is the subset of app.Service that the HTTP handlers
// dispatch to. A small wrapper in the CLI layer adapts *app.Service to
// this interface, which keeps the api package decoupled from app (no
// import cycle risk) and makes handler tests trivial to stub.
//
// CreateSession and LaunchSession are separate steps per the v0.0.2
// context-pack §08 API shape: POST /sessions creates (state=created),
// POST /sessions/{id}/launch starts. Clients that want the combined
// flow issue both calls — the common case isn't common enough to
// warrant a shortcut endpoint in v0.0.2.
type LaunchService interface {
	CreateSession(launchID string) (LaunchResult, error)
	CreateSessionWithBootPrompt(launchID, bootPrompt string) (LaunchResult, error)
	// CreateSessionWithInput is the v005-08 Agent Ops entry point that accepts
	// Tier-2 caller-provided agent/boot-profile/override payloads. The legacy
	// CreateSession / CreateSessionWithBootPrompt methods route through it
	// internally, so handlers that want either behavior can dispatch here.
	CreateSessionWithInput(in CreateSessionInput) (LaunchResult, error)
	LaunchSession(sessionID string) (LaunchResult, error)
	ListSessions(opts store.ListSessionsOptions) ([]store.SessionRow, error)
	GetSession(id string) (*store.SessionRow, error)
	StopSession(id string) error
	WaitSession(ctx context.Context, id string) (int, error)
	SendInput(id string, data []byte) error
	SendTurn(ctx context.Context, id, text string) error
	AttachSession(ctx context.Context, id string, w io.Writer, sinceSeq int64) error
	AttachedClients(id string) int
	ResizeSession(id string, rows, cols uint16) error
	// ResumeLogicalAgent starts a new session for the agent using its most
	// recent checkpoint as boot context. Returns the new session's launch
	// result. Errors: not_found if no checkpoint exists; conflict if the
	// agent has never had a session launched (no launch_id).
	ResumeLogicalAgent(logicalAgentID string) (LaunchResult, error)
	// RuntimeHealth returns the live health snapshot for a running session.
	// Returns (zero, false) when the session is not currently registered
	// in the runtime manager (never launched, already terminal, or unknown).
	RuntimeHealth(id string) (RuntimeHealthResult, bool)
}

// RuntimeHealthResult is the api-facing health snapshot. It carries the
// live HealthStatus from the agentsessions.Session and the static
// Capabilities from the agentsessions.Runtime that spawned it, along
// with provider identity.
type RuntimeHealthResult struct {
	SessionID    string
	ProviderID   string
	ProviderKind string
	Caps         agentsessions.Capabilities
	Health       agentsessions.HealthStatus
}

// LaunchResult is the api-facing subset of app.Launched. The full app
// struct carries a *workspace.Session and a Wait closure; the API only
// needs the primitive strings for the response body.
type LaunchResult struct {
	SessionID  string
	Workspace  string
	LogPath    string
	ProviderID string
	// ProviderKind is the runtime family ("cli" | "api"). Consumers may
	// branch on this to select PTY-specific affordances (resize, raw input)
	// vs. API-mode affordances (structured turns). Sourced from
	// agentsessions.Runtime.Kind().
	ProviderKind string
	// LogicalAgentID is the durable identity that accumulates checkpoints
	// across sessions. Returned on create and launch so consumers can
	// correlate a new session to its logical agent without a follow-up get.
	LogicalAgentID string
}

// (RuntimeKind / Capabilities live in agentsessions; see
// agentsessions.Capabilities for the canonical type.)

// Request / response payloads for the HTTP API. JSON tags are the public
// contract; rename with care.

type LaunchRequest struct {
	Launch string `json:"launch"`
	// BootPrompt, when non-empty, overrides the catalog's static boot prompt
	// fragments. Used by `mux boot <profile_id>` to inject a dynamically
	// generated boot prompt without modifying the catalog.
	BootPrompt string `json:"boot_prompt,omitempty"`

	// v005-08 Agent Ops Tier-2 caller-provided payload fields. All optional.
	// Resolve order for agent: AgentInline > AgentFile > catalog agent.
	AgentFile       string `json:"agent_file,omitempty"`
	AgentInline     string `json:"agent_inline,omitempty"`
	BootProfileFile string `json:"boot_profile,omitempty"`
	Override        string `json:"override,omitempty"`
	PromptAppend    string `json:"prompt_append,omitempty"`

	// Injection is a JSON-encoded config.LaunchInjection: caller-provided
	// native files + boot-dir overlay supplied outside catalog YAML. Caller
	// native files are appended after catalog native files; caller boot-dir
	// overlay entries win over catalog entries on a duplicate rel_path.
	//
	// SECURITY: injected content is persisted at rest in launch_plans —
	// non-secret-only. See config.LaunchInjection.
	Injection string `json:"injection,omitempty"`
}

// CreateSessionInput mirrors app.CreateSessionInput in shape but is defined
// here so the api package doesn't import internal/app (which would create a
// cycle via internal/app importing api). The adapter in cmd/mux/daemon.go
// translates between the two.
type CreateSessionInput struct {
	LaunchID           string
	BootPromptOverride string
	AgentFile          string
	AgentInline        string
	BootProfileFile    string
	Override           string
	PromptAppend       string
	// Injection is a JSON-encoded config.LaunchInjection. See LaunchRequest.
	Injection string
}

type LaunchResponse struct {
	ID             string `json:"id"`
	Workspace      string `json:"workspace"`
	Log            string `json:"log"`
	ProviderID     string `json:"provider_id"`
	ProviderKind   string `json:"provider_kind"`
	LogicalAgentID string `json:"logical_agent_id"`
}

type WaitResponse struct {
	ExitCode int `json:"exit_code"`
}

// CapabilitiesDTO is the on-the-wire representation of agentsessions.Capabilities.
type CapabilitiesDTO struct {
	PTY               bool `json:"pty"`
	StreamingStdio    bool `json:"streaming_stdio"`
	JSONRPCStdio      bool `json:"jsonrpc_stdio"`
	Resize            bool `json:"resize"`
	ProviderSessionID bool `json:"provider_session_id"`
	CheckpointResume  bool `json:"checkpoint_resume"`
	BinaryRequired    bool `json:"binary_required"`
}

// RuntimeHealthResponse is the body of GET /sessions/{id}/health.
type RuntimeHealthResponse struct {
	SessionID    string          `json:"session_id"`
	Alive        bool            `json:"alive"`
	PID          int             `json:"pid,omitempty"`
	LiveState    string          `json:"live_state"`
	TurnID       string          `json:"turn_id,omitempty"`
	ProviderID   string          `json:"provider_id"`
	ProviderKind string          `json:"provider_kind"`
	Caps         CapabilitiesDTO `json:"caps"`
}

// ResizeRequest is the body of POST /sessions/{id}/resize. Rows and
// Cols are required and must be > 0; the endpoint rejects zero-valued
// dimensions with invalid_request so callers can't accidentally shrink
// a session to a no-op winsize.
type ResizeRequest struct {
	Rows uint16 `json:"rows"`
	Cols uint16 `json:"cols"`
}

type ListSessionsResponse struct {
	Sessions []SessionDTO `json:"sessions"`
	// NextCursor is set when the returned page hit the limit, indicating
	// more rows may exist older than this point. Empty when the caller
	// has reached the end (or the query was unfiltered and under limit).
	NextCursor string `json:"next_cursor,omitempty"`
}

type SessionDTO struct {
	ID             string `json:"id"`
	LaunchID       string `json:"launch_id"`
	ProjectID      string `json:"project_id"`
	LogicalAgentID string `json:"logical_agent_id"`
	ProviderID     string `json:"provider_id"`
	// ProviderKind is the runtime family ("cli" | "api"). Consumers branch
	// on this to select PTY-specific affordances vs. API-mode affordances.
	// Empty for sessions created before this field was added (pre-v0.0.3).
	ProviderKind    string  `json:"provider_kind,omitempty"`
	Workspace       string  `json:"workspace"`
	State           string  `json:"state"`
	PID             *int    `json:"pid,omitempty"`
	ExitCode        *int    `json:"exit_code,omitempty"`
	CreatedAt       string  `json:"created_at"`
	UpdatedAt       string  `json:"updated_at"`
	EndedAt         *string `json:"ended_at,omitempty"`
	AttachedClients int     `json:"attached_clients"`
	SessionGroupID  string  `json:"session_group_id,omitempty"`
}

// AttachmentDTO is the on-the-wire shape for a client attachment row.
// DetachedAt is "" when the row has no detached_at stamp (attachment
// still open or predates detach tracking).
type AttachmentDTO struct {
	ID         string `json:"id"`
	SessionID  string `json:"session_id"`
	ClientKind string `json:"client_kind"`
	AttachedAt string `json:"attached_at"`
	DetachedAt string `json:"detached_at"`
}

// AttachmentListResponse is the collection response for
// GET /sessions/{id}/attachments.
type AttachmentListResponse struct {
	Attachments []AttachmentDTO `json:"attachments"`
	Count       int             `json:"count"`
}

// SessionRowToDTO flattens the sql.Null* fields on store.SessionRow into
// pointer-valued JSON-friendly shapes. Nil means "no value set" (e.g.,
// session not yet running, or hasn't exited).
func SessionRowToDTO(r store.SessionRow) SessionDTO {
	dto := SessionDTO{
		ID:             r.ID,
		LaunchID:       r.LaunchID,
		ProjectID:      r.ProjectID,
		LogicalAgentID: r.LogicalAgentID,
		ProviderID:     r.ProviderID,
		ProviderKind:   r.ProviderKind,
		Workspace:      r.Workspace,
		State:          r.State,
		CreatedAt:      r.CreatedAt,
		UpdatedAt:      r.UpdatedAt,
	}
	if r.PID.Valid {
		v := int(r.PID.Int64)
		dto.PID = &v
	}
	if r.ExitCode.Valid {
		v := int(r.ExitCode.Int64)
		dto.ExitCode = &v
	}
	if r.EndedAt.Valid {
		s := r.EndedAt.String
		dto.EndedAt = &s
	}
	dto.SessionGroupID = r.SessionGroupID.String
	return dto
}
