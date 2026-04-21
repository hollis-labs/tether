package api

import (
	"context"
	"io"

	"github.com/chrispian/agent-mux/internal/store"
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
	LaunchSession(sessionID string) (LaunchResult, error)
	ListSessions(opts store.ListSessionsOptions) ([]store.SessionRow, error)
	GetSession(id string) (*store.SessionRow, error)
	StopSession(id string) error
	WaitSession(ctx context.Context, id string) (int, error)
	SendInput(id string, data []byte) error
	AttachSession(ctx context.Context, id string, w io.Writer, sinceSeq int64) error
	AttachedClients(id string) int
	ResizeSession(id string, rows, cols uint16) error
	// ResumeLogicalAgent starts a new session for the agent using its most
	// recent checkpoint as boot context. Returns the new session's launch
	// result. Errors: not_found if no checkpoint exists; conflict if the
	// agent has never had a session launched (no launch_id).
	ResumeLogicalAgent(logicalAgentID string) (LaunchResult, error)
}

// LaunchResult is the api-facing subset of app.Launched. The full app
// struct carries a *workspace.Session and a Wait closure; the API only
// needs the primitive strings for the response body.
type LaunchResult struct {
	SessionID  string
	Workspace  string
	LogPath    string
	ProviderID string
}

// Request / response payloads for the HTTP API. JSON tags are the public
// contract; rename with care.

type LaunchRequest struct {
	Launch string `json:"launch"`
}

type LaunchResponse struct {
	ID         string `json:"id"`
	Workspace  string `json:"workspace"`
	Log        string `json:"log"`
	ProviderID string `json:"provider_id"`
}

type WaitResponse struct {
	ExitCode int `json:"exit_code"`
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
	ID              string  `json:"id"`
	LaunchID        string  `json:"launch_id"`
	ProjectID       string  `json:"project_id"`
	LogicalAgentID  string  `json:"logical_agent_id"`
	ProviderID      string  `json:"provider_id"`
	Workspace       string  `json:"workspace"`
	State           string  `json:"state"`
	PID             *int    `json:"pid,omitempty"`
	ExitCode        *int    `json:"exit_code,omitempty"`
	CreatedAt       string  `json:"created_at"`
	UpdatedAt       string  `json:"updated_at"`
	EndedAt         *string `json:"ended_at,omitempty"`
	AttachedClients int     `json:"attached_clients"`
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
	return dto
}
