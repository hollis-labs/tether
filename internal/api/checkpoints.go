package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/hollis-labs/tether/internal/checkpoint"
	"github.com/hollis-labs/tether/internal/store"
)

// CheckpointStore is the narrow storage seam the checkpoint handlers
// depend on. *store.Store satisfies it directly; tests pass a fake.
// Intentionally smaller than LaunchService to keep concerns separate —
// checkpoints are pure persistence in v0.0.2 (no runtime side effects
// until v0.0.3 wires the resume flow).
type CheckpointStore interface {
	CreateCheckpoint(c checkpoint.Checkpoint) error
	ListCheckpointsByLogicalAgent(logicalAgentID string) ([]checkpoint.Checkpoint, error)
	GetLatestCheckpointForAgent(logicalAgentID string) (*checkpoint.Checkpoint, error)
	ListLogicalAgents() ([]store.LogicalAgentRow, error)
}

// LogicalAgentSummary is the minimal projection the TUI needs to display
// logical agents and decide whether resume is available.
type LogicalAgentSummary struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	LaunchID string `json:"launch_id,omitempty"`
}

// LogicalAgentListResponse is the collection response for GET /logical-agents.
type LogicalAgentListResponse struct {
	Agents []LogicalAgentSummary `json:"agents"`
}

// CheckpointCreateRequest mirrors the free-form shape accepted by
// POST /sessions/{id}/checkpoint. Every field is optional — v0.0.2
// does not pin a rigid schema; consumers (Nanite, Clockwork) set
// only what they need and the rest persists as SQL NULL.
//
// Server-set fields (ID, LogicalAgentID, CreatedAt, SourceSessionID)
// are not accepted from the request body.
type CheckpointCreateRequest struct {
	TaskID              string `json:"task_id,omitempty"`
	WorkflowID          string `json:"workflow_id,omitempty"`
	Status              string `json:"status,omitempty"`
	CompletedWork       string `json:"completed_work,omitempty"`
	PendingWork         string `json:"pending_work,omitempty"`
	KeyDecisions        string `json:"key_decisions,omitempty"`
	ReferencedArtifacts string `json:"referenced_artifacts,omitempty"`
	Summary             string `json:"summary,omitempty"`
	NextRecommendation  string `json:"next_recommendation,omitempty"`
}

// CheckpointDTO is the on-the-wire shape for a checkpoint row. Mirrors
// checkpoint.Checkpoint 1:1 with json tags for the public contract.
type CheckpointDTO struct {
	ID                  string `json:"id"`
	LogicalAgentID      string `json:"logical_agent_id"`
	TaskID              string `json:"task_id,omitempty"`
	WorkflowID          string `json:"workflow_id,omitempty"`
	Status              string `json:"status,omitempty"`
	CompletedWork       string `json:"completed_work,omitempty"`
	PendingWork         string `json:"pending_work,omitempty"`
	KeyDecisions        string `json:"key_decisions,omitempty"`
	ReferencedArtifacts string `json:"referenced_artifacts,omitempty"`
	Summary             string `json:"summary,omitempty"`
	NextRecommendation  string `json:"next_recommendation,omitempty"`
	CreatedAt           string `json:"created_at"`
	SourceSessionID     string `json:"source_session_id,omitempty"`
}

type CheckpointListResponse struct {
	Checkpoints []CheckpointDTO `json:"checkpoints"`
}

func checkpointToDTO(c checkpoint.Checkpoint) CheckpointDTO {
	return CheckpointDTO{
		ID:                  c.ID,
		LogicalAgentID:      c.LogicalAgentID,
		TaskID:              c.TaskID,
		WorkflowID:          c.WorkflowID,
		Status:              c.Status,
		CompletedWork:       c.CompletedWork,
		PendingWork:         c.PendingWork,
		KeyDecisions:        c.KeyDecisions,
		ReferencedArtifacts: c.ReferencedArtifacts,
		Summary:             c.Summary,
		NextRecommendation:  c.NextRecommendation,
		CreatedAt:           c.CreatedAt,
		SourceSessionID:     c.SourceSessionID,
	}
}

// registerCheckpointRoutes wires the checkpoint and logical-agent
// checkpoint routes onto mux. No-op when either dependency is nil —
// lets tests build partial servers.
func (s *Server) registerCheckpointRoutes(mux *http.ServeMux) {
	if s.Service == nil || s.Checkpoints == nil {
		return
	}
	// POST /sessions/{id}/checkpoint is parameterised but shares the
	// /sessions/ prefix owned by the sessions handler — it's dispatched
	// from handleSessionsItem's action switch.
	mux.HandleFunc("/logical-agents", s.handleLogicalAgentsCollection)
	mux.HandleFunc("/logical-agents/", s.handleLogicalAgentsItem)
}

// handleLogicalAgentsCollection handles GET /logical-agents.
func (s *Server) handleLogicalAgentsCollection(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
		return
	}
	rows, err := s.Checkpoints.ListLogicalAgents()
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}
	out := make([]LogicalAgentSummary, 0, len(rows))
	for _, r := range rows {
		out = append(out, LogicalAgentSummary{
			ID:       r.ID,
			Name:     r.Name,
			LaunchID: r.LaunchID,
		})
	}
	writeJSON(w, http.StatusOK, LogicalAgentListResponse{Agents: out})
}

// handleLogicalAgentsItem dispatches the two /logical-agents/{id}/...
// paths registered here. Hand-rolled routing mirrors the /sessions
// handler style to avoid pulling in a router dependency.
func (s *Server) handleLogicalAgentsItem(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/logical-agents/")
	if rest == "" {
		writeError(w, http.StatusNotFound, CodeNotFound, "logical agent id required")
		return
	}
	parts := strings.SplitN(rest, "/", 2)
	id := parts[0]
	action := ""
	if len(parts) == 2 {
		action = parts[1]
	}
	switch action {
	case "checkpoints":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
			return
		}
		s.handleListCheckpoints(w, r, id)
	case "resume":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
			return
		}
		s.handleResumeLogicalAgent(w, r, id)
	default:
		writeError(w, http.StatusNotFound, CodeNotFound, "unknown action "+action)
	}
}

// handleCreateCheckpoint services POST /sessions/{id}/checkpoint. The
// session is read to resolve its logical_agent_id (the checkpoint's
// owning entity); the ID is server-assigned (UUIDv7 for sortable
// creation ordering per boot-prompt critical note #9).
//
// No runtime side effects in v0.0.2 — the session is not paused,
// context is not snapshotted. v0.0.3 Sprint v003-04 will wire those.
func (s *Server) handleCreateCheckpoint(w http.ResponseWriter, r *http.Request, sessionID string) {
	// Body is optional — POST with an empty body creates a minimal
	// checkpoint row. Decoder errors still surface as 400 so clients
	// know they sent malformed JSON.
	var req CheckpointCreateRequest
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, "invalid request body: "+err.Error())
			return
		}
	}

	row, err := s.Service.GetSession(sessionID)
	if err != nil {
		if errors.Is(err, store.ErrSessionNotFound) {
			writeError(w, http.StatusNotFound, CodeNotFound, "session not found")
			return
		}
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}

	id, err := uuid.NewV7()
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, "generate id: "+err.Error())
		return
	}

	c := checkpoint.Checkpoint{
		ID:                  id.String(),
		LogicalAgentID:      row.LogicalAgentID,
		TaskID:              req.TaskID,
		WorkflowID:          req.WorkflowID,
		Status:              req.Status,
		CompletedWork:       req.CompletedWork,
		PendingWork:         req.PendingWork,
		KeyDecisions:        req.KeyDecisions,
		ReferencedArtifacts: req.ReferencedArtifacts,
		Summary:             req.Summary,
		NextRecommendation:  req.NextRecommendation,
		CreatedAt:           time.Now().UTC().Format(time.RFC3339),
		SourceSessionID:     sessionID,
	}

	if err := s.Checkpoints.CreateCheckpoint(c); err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, checkpointToDTO(c))
}

// handleListCheckpoints services GET /logical-agents/{id}/checkpoints.
// Returns newest-first per the store's ordering contract.
func (s *Server) handleListCheckpoints(w http.ResponseWriter, _ *http.Request, agentID string) {
	rows, err := s.Checkpoints.ListCheckpointsByLogicalAgent(agentID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}
	out := make([]CheckpointDTO, 0, len(rows))
	for _, r := range rows {
		out = append(out, checkpointToDTO(r))
	}
	writeJSON(w, http.StatusOK, CheckpointListResponse{Checkpoints: out})
}

// handleResumeLogicalAgent services POST /logical-agents/{id}/resume.
// Starts a new session using the agent's most recent checkpoint as boot context.
// The agent's stored launch_id (set on its most recent LaunchSession call) is
// reused — no launch override is supported in v0.0.4. Body is ignored.
func (s *Server) handleResumeLogicalAgent(w http.ResponseWriter, _ *http.Request, agentID string) {
	res, err := s.Service.ResumeLogicalAgent(agentID)
	if err != nil {
		msg := err.Error()
		switch {
		case errors.Is(err, store.ErrSessionNotFound),
			strings.Contains(msg, "no checkpoint"),
			strings.Contains(msg, "no rows"):
			writeError(w, http.StatusNotFound, CodeNotFound, "no checkpoint found for agent "+agentID)
		case strings.Contains(msg, "never launched"):
			writeError(w, http.StatusConflict, CodeConflict, msg)
		default:
			writeError(w, http.StatusInternalServerError, CodeInternalError, msg)
		}
		return
	}
	writeJSON(w, http.StatusCreated, LaunchResponse{
		ID:             res.SessionID,
		Workspace:      res.Workspace,
		Log:            res.LogPath,
		ProviderID:     res.ProviderID,
		ProviderKind:   res.ProviderKind,
		LogicalAgentID: res.LogicalAgentID,
	})
}
