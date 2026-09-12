package api

// workstreams.go — HTTP surface for the workstream container.
//
// S1 of SP-20260912-0001 (CW-20260912-0059). The store layer and the reason
// the object exists are in internal/store/workstreams.go and migration 0023.

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/hollis-labs/tether/internal/store"
)

// WorkstreamStore is the storage seam for workstream handlers.
// *store.Store satisfies it.
type WorkstreamStore interface {
	CreateWorkstream(w store.WorkstreamRow) (store.WorkstreamRow, error)
	GetWorkstream(id string) (*store.WorkstreamRow, error)
	ListWorkstreams(opts store.ListWorkstreamsOptions) ([]store.WorkstreamRow, error)
	AssignSessionWorkstream(sessionID, workstreamID string) error
	EnsureSessionWorkstream(sessionID string, seed store.WorkstreamRow) (store.WorkstreamRow, error)
}

// WorkstreamDTO is the wire shape for a workstream.
type WorkstreamDTO struct {
	ID         string `json:"id"`
	Name       string `json:"name,omitempty"`
	WorkflowID string `json:"workflow_id,omitempty"`
	Status     string `json:"status"`
	CreatedAt  string `json:"created_at"`
	UpdatedAt  string `json:"updated_at"`
}

// WorkstreamListResponse is the collection response for GET /workstreams.
type WorkstreamListResponse struct {
	Workstreams []WorkstreamDTO `json:"workstreams"`
}

// WorkstreamCreateRequest is the JSON body for POST /workstreams.
type WorkstreamCreateRequest struct {
	Name       string `json:"name,omitempty"`
	WorkflowID string `json:"workflow_id,omitempty"`
}

// WorkstreamAssignRequest is the JSON body for
// POST /workstreams/{id}/sessions. An empty workstream id in the path is not
// how you clear an assignment -- use POST /sessions/{id}/workstream with an
// empty workstream_id for that.
type WorkstreamAssignRequest struct {
	SessionID string `json:"session_id"`
}

// SessionWorkstreamRequest is the JSON body for
// POST /sessions/{id}/workstream. An empty WorkstreamID clears the
// association; Ensure asks for one to be created when the lineage has none.
type SessionWorkstreamRequest struct {
	WorkstreamID string `json:"workstream_id,omitempty"`
	Ensure       bool   `json:"ensure,omitempty"`
	Name         string `json:"name,omitempty"`
	WorkflowID   string `json:"workflow_id,omitempty"`
}

func workstreamToDTO(w store.WorkstreamRow) WorkstreamDTO {
	return WorkstreamDTO{
		ID:         w.ID,
		Name:       w.Name,
		WorkflowID: w.WorkflowID,
		Status:     w.Status,
		CreatedAt:  w.CreatedAt,
		UpdatedAt:  w.UpdatedAt,
	}
}

// registerWorkstreamRoutes mounts workstream endpoints. No-op when
// Workstreams is nil, matching registerSessionGroupRoutes so tests that do not
// need them can skip the wiring.
func (s *Server) registerWorkstreamRoutes(mux *http.ServeMux) {
	if s.Workstreams == nil {
		return
	}
	mux.HandleFunc("/workstreams", s.handleWorkstreamsCollection)
	mux.HandleFunc("/workstreams/", s.handleWorkstreamsItem)
}

func (s *Server) handleWorkstreamsCollection(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		s.handleCreateWorkstream(w, r)
	case http.MethodGet:
		s.handleListWorkstreams(w, r)
	default:
		writeError(w, http.StatusMethodNotAllowed, CodeInvalidRequest, "method not allowed")
	}
}

func (s *Server) handleCreateWorkstream(w http.ResponseWriter, r *http.Request) {
	var req WorkstreamCreateRequest
	if r.Body != nil {
		// An empty body is a valid "just give me a container" create, so a
		// decode failure on a genuinely empty body is not an error.
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, "invalid JSON body: "+err.Error())
			return
		}
	}
	created, err := s.Workstreams.CreateWorkstream(store.WorkstreamRow{
		Name:       req.Name,
		WorkflowID: req.WorkflowID,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, workstreamToDTO(created))
}

func (s *Server) handleListWorkstreams(w http.ResponseWriter, r *http.Request) {
	opts := store.ListWorkstreamsOptions{
		Status:     r.URL.Query().Get("status"),
		WorkflowID: r.URL.Query().Get("workflow_id"),
	}
	rows, err := s.Workstreams.ListWorkstreams(opts)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}
	out := make([]WorkstreamDTO, 0, len(rows))
	for _, row := range rows {
		out = append(out, workstreamToDTO(row))
	}
	writeJSON(w, http.StatusOK, WorkstreamListResponse{Workstreams: out})
}

func (s *Server) handleWorkstreamsItem(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/workstreams/")
	id, sub, _ := strings.Cut(rest, "/")
	if id == "" {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "workstream id is required")
		return
	}
	switch {
	case sub == "" && r.Method == http.MethodGet:
		s.handleGetWorkstream(w, id)
	case sub == "sessions" && r.Method == http.MethodPost:
		s.handleAssignWorkstream(w, r, id)
	default:
		writeError(w, http.StatusMethodNotAllowed, CodeInvalidRequest, "method not allowed")
	}
}

func (s *Server) handleGetWorkstream(w http.ResponseWriter, id string) {
	row, err := s.Workstreams.GetWorkstream(id)
	if err != nil {
		if errors.Is(err, store.ErrWorkstreamNotFound) {
			writeError(w, http.StatusNotFound, CodeNotFound, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, workstreamToDTO(*row))
}

func (s *Server) handleAssignWorkstream(w http.ResponseWriter, r *http.Request, workstreamID string) {
	var req WorkstreamAssignRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "invalid JSON body: "+err.Error())
		return
	}
	if req.SessionID == "" {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "session_id is required")
		return
	}
	if err := s.Workstreams.AssignSessionWorkstream(req.SessionID, workstreamID); err != nil {
		writeAssignError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleSessionWorkstream backs POST /sessions/{id}/workstream: assign, clear,
// or ensure-on-demand for one session.
func (s *Server) handleSessionWorkstream(w http.ResponseWriter, r *http.Request, sessionID string) {
	if s.Workstreams == nil {
		writeError(w, http.StatusNotFound, CodeNotFound, "workstreams are not enabled on this server")
		return
	}
	var req SessionWorkstreamRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "invalid JSON body: "+err.Error())
		return
	}
	if req.Ensure {
		if req.WorkstreamID != "" {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, "ensure and workstream_id are mutually exclusive")
			return
		}
		ws, err := s.Workstreams.EnsureSessionWorkstream(sessionID, store.WorkstreamRow{
			Name:       req.Name,
			WorkflowID: req.WorkflowID,
		})
		if err != nil {
			writeAssignError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, workstreamToDTO(ws))
		return
	}
	if err := s.Workstreams.AssignSessionWorkstream(sessionID, req.WorkstreamID); err != nil {
		writeAssignError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeAssignError maps the two not-found cases the assign/ensure paths can
// produce onto 404 and everything else onto 500.
func writeAssignError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrWorkstreamNotFound), errors.Is(err, store.ErrSessionNotFound):
		writeError(w, http.StatusNotFound, CodeNotFound, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
	}
}
