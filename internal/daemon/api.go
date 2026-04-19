package daemon

import (
	"context"
	"io"

	"github.com/chrispian/agent-mux/internal/store"
)

// LaunchService is the subset of app.Service that the daemon's HTTP surface
// calls. A small wrapper in the CLI layer adapts *app.Service to this
// interface, which keeps the daemon package decoupled from app (no import
// cycle risk) and makes handler tests trivial to stub.
type LaunchService interface {
	Launch(launchID string) (LaunchResult, error)
	ListSessions() ([]store.SessionRow, error)
	GetSession(id string) (*store.SessionRow, error)
	StopSession(id string) error
	WaitSession(ctx context.Context, id string) (int, error)
	SendInput(id string, data []byte) error
	AttachSession(ctx context.Context, id string, w io.Writer) error
	AttachedClients(id string) int
}

// LaunchResult is the daemon-facing subset of app.Launched. The full app
// struct carries a *workspace.Session and a Wait closure; the API only
// needs the primitive strings for the response body.
type LaunchResult struct {
	SessionID string
	Workspace string
	LogPath   string
}

// Request / response payloads for the HTTP API. JSON tags are the public
// contract; rename with care.

type LaunchRequest struct {
	Launch string `json:"launch"`
}

type LaunchResponse struct {
	ID        string `json:"id"`
	Workspace string `json:"workspace"`
	Log       string `json:"log"`
}

type WaitResponse struct {
	ExitCode int `json:"exit_code"`
}

type ListSessionsResponse struct {
	Sessions []SessionDTO `json:"sessions"`
}

type SessionDTO struct {
	ID              string  `json:"id"`
	LaunchID        string  `json:"launch_id"`
	ProjectID       string  `json:"project_id"`
	AgentID         string  `json:"agent_id"`
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

type ErrorResponse struct {
	Error string `json:"error"`
}

// SessionRowToDTO flattens the sql.Null* fields on store.SessionRow into
// pointer-valued JSON-friendly shapes. Nil means "no value set" (e.g.,
// session not yet running, or hasn't exited).
func SessionRowToDTO(r store.SessionRow) SessionDTO {
	dto := SessionDTO{
		ID:         r.ID,
		LaunchID:   r.LaunchID,
		ProjectID:  r.ProjectID,
		AgentID:    r.AgentID,
		ProviderID: r.ProviderID,
		Workspace:  r.Workspace,
		State:      r.State,
		CreatedAt:  r.CreatedAt,
		UpdatedAt:  r.UpdatedAt,
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
