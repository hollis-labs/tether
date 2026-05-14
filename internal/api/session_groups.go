package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/hollis-labs/tether/internal/store"
)

// SessionGroupStore is the storage seam for session group handlers.
// *store.Store satisfies it.
type SessionGroupStore interface {
	CreateSessionGroup(g store.SessionGroupRow) error
	GetSessionGroup(id string) (*store.SessionGroupRow, error)
	ListSessionGroups() ([]store.SessionGroupRow, error)
	AddSessionToGroup(sessionID, groupID string) error
	ListSessionsByGroup(groupID string) ([]store.SessionRow, error)
}

// SessionGroupDTO is the wire shape for a session group.
type SessionGroupDTO struct {
	ID         string `json:"id"`
	Name       string `json:"name,omitempty"`
	WorkflowID string `json:"workflow_id,omitempty"`
	CreatedAt  string `json:"created_at"`
}

// SessionGroupListResponse is the collection response for GET /session-groups.
type SessionGroupListResponse struct {
	Groups []SessionGroupDTO `json:"groups"`
}

// SessionGroupCreateRequest is the JSON body for POST /session-groups.
type SessionGroupCreateRequest struct {
	Name       string `json:"name,omitempty"`
	WorkflowID string `json:"workflow_id,omitempty"`
}

// GroupMemberRequest is the JSON body for POST /session-groups/{id}/members.
type GroupMemberRequest struct {
	SessionID string `json:"session_id"`
}

func groupToDTO(g store.SessionGroupRow) SessionGroupDTO {
	return SessionGroupDTO{
		ID:         g.ID,
		Name:       g.Name,
		WorkflowID: g.WorkflowID,
		CreatedAt:  g.CreatedAt,
	}
}

// registerSessionGroupRoutes mounts session group endpoints. No-op when
// GroupStore is nil so tests that don't need groups can skip wiring it.
func (s *Server) registerSessionGroupRoutes(mux *http.ServeMux) {
	if s.GroupStore == nil {
		return
	}
	mux.HandleFunc("/session-groups", s.handleSessionGroupsCollection)
	mux.HandleFunc("/session-groups/", s.handleSessionGroupsItem)
}

func (s *Server) handleSessionGroupsCollection(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		s.handleCreateSessionGroup(w, r)
	case http.MethodGet:
		s.handleListSessionGroups(w, r)
	default:
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) handleSessionGroupsItem(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/session-groups/")
	if rest == "" {
		writeError(w, http.StatusNotFound, CodeNotFound, "group id required")
		return
	}
	parts := strings.SplitN(rest, "/", 2)
	id := parts[0]
	action := ""
	if len(parts) == 2 {
		action = parts[1]
	}

	switch action {
	case "":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
			return
		}
		s.handleGetSessionGroup(w, r, id)
	case "sessions":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
			return
		}
		s.handleListGroupSessions(w, r, id)
	case "members":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
			return
		}
		s.handleAddGroupMember(w, r, id)
	default:
		writeError(w, http.StatusNotFound, CodeNotFound, "unknown action "+action)
	}
}

func (s *Server) handleCreateSessionGroup(w http.ResponseWriter, r *http.Request) {
	var req SessionGroupCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "invalid request body: "+err.Error())
		return
	}
	id, err := uuid.NewV7()
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, "generate id: "+err.Error())
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	g := store.SessionGroupRow{
		ID:         id.String(),
		Name:       req.Name,
		WorkflowID: req.WorkflowID,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := s.GroupStore.CreateSessionGroup(g); err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, groupToDTO(g))
}

func (s *Server) handleListSessionGroups(w http.ResponseWriter, _ *http.Request) {
	groups, err := s.GroupStore.ListSessionGroups()
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}
	out := make([]SessionGroupDTO, 0, len(groups))
	for _, g := range groups {
		out = append(out, groupToDTO(g))
	}
	writeJSON(w, http.StatusOK, SessionGroupListResponse{Groups: out})
}

func (s *Server) handleGetSessionGroup(w http.ResponseWriter, _ *http.Request, id string) {
	g, err := s.GroupStore.GetSessionGroup(id)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			writeError(w, http.StatusNotFound, CodeNotFound, "group not found")
			return
		}
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, groupToDTO(*g))
}

func (s *Server) handleListGroupSessions(w http.ResponseWriter, _ *http.Request, groupID string) {
	rows, err := s.GroupStore.ListSessionsByGroup(groupID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}
	dtos := make([]SessionDTO, 0, len(rows))
	for _, r := range rows {
		dtos = append(dtos, SessionRowToDTO(r))
	}
	writeJSON(w, http.StatusOK, ListSessionsResponse{Sessions: dtos})
}

func (s *Server) handleAddGroupMember(w http.ResponseWriter, r *http.Request, groupID string) {
	var req GroupMemberRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "invalid request body: "+err.Error())
		return
	}
	if req.SessionID == "" {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "session_id required")
		return
	}
	if err := s.GroupStore.AddSessionToGroup(req.SessionID, groupID); err != nil {
		msg := err.Error()
		if strings.Contains(msg, "no session row") {
			writeError(w, http.StatusNotFound, CodeNotFound, "session not found")
			return
		}
		writeError(w, http.StatusInternalServerError, CodeInternalError, msg)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
