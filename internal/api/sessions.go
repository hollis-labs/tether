package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/chrispian/agent-mux/internal/runtime"
)

// registerSessionRoutes wires /sessions handlers onto mux. Route matching
// is hand-rolled: two static collection paths, a few parameterised item
// actions. Introducing a third-party router would be overkill at this
// size.
func (s *Server) registerSessionRoutes(mux *http.ServeMux) {
	if s.Service == nil {
		return
	}
	mux.HandleFunc("/sessions", s.handleSessionsCollection)
	mux.HandleFunc("/sessions/", s.handleSessionsItem)
}

func (s *Server) handleSessionsCollection(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleListSessions(w, r)
	case http.MethodPost:
		s.handleLaunch(w, r)
	default:
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) handleSessionsItem(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/sessions/")
	if rest == "" {
		writeError(w, http.StatusNotFound, CodeNotFound, "session id required")
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
		s.handleGetSession(w, r, id)
	case "launch":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
			return
		}
		s.handleLaunchSession(w, r, id)
	case "stop":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
			return
		}
		s.handleStopSession(w, r, id)
	case "wait":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
			return
		}
		s.handleWaitSession(w, r, id)
	case "input":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
			return
		}
		s.handleSendInput(w, r, id)
	case "attach":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
			return
		}
		s.handleAttach(w, r, id)
	default:
		writeError(w, http.StatusNotFound, CodeNotFound, "unknown action "+action)
	}
}

// handleLaunch services POST /sessions — the create leg of the split.
// Prepares workspace + persists the session row in state=created, but
// does NOT start the runtime. Callers subsequently POST to
// /sessions/{id}/launch to start. Returns 201.
func (s *Server) handleLaunch(w http.ResponseWriter, r *http.Request) {
	var req LaunchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "invalid request body: "+err.Error())
		return
	}
	if req.Launch == "" {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "launch id required")
		return
	}
	res, err := s.Service.CreateSession(req.Launch)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, LaunchResponse{
		ID:        res.SessionID,
		Workspace: res.Workspace,
		Log:       res.LogPath,
	})
}

// handleLaunchSession services POST /sessions/{id}/launch — the start
// leg of the split. Returns 200 on successful transition to running,
// 409 when the session is not in 'created' state, 404 when not found.
func (s *Server) handleLaunchSession(w http.ResponseWriter, _ *http.Request, id string) {
	res, err := s.Service.LaunchSession(id)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			writeError(w, http.StatusNotFound, CodeNotFound, "session not found")
			return
		}
		if strings.Contains(err.Error(), "not in 'created' state") {
			writeError(w, http.StatusConflict, CodeConflict, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, LaunchResponse{
		ID:        res.SessionID,
		Workspace: res.Workspace,
		Log:       res.LogPath,
	})
}

func (s *Server) handleListSessions(w http.ResponseWriter, _ *http.Request) {
	rows, err := s.Service.ListSessions()
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}
	out := make([]SessionDTO, 0, len(rows))
	for _, row := range rows {
		dto := SessionRowToDTO(row)
		dto.AttachedClients = s.Service.AttachedClients(row.ID)
		out = append(out, dto)
	}
	writeJSON(w, http.StatusOK, ListSessionsResponse{Sessions: out})
}

func (s *Server) handleGetSession(w http.ResponseWriter, _ *http.Request, id string) {
	row, err := s.Service.GetSession(id)
	if err != nil {
		// Store returns sql.ErrNoRows for missing; surface as 404 regardless
		// of the specific driver error (we don't want to import database/sql
		// just to type-check).
		if strings.Contains(err.Error(), "no rows") {
			writeError(w, http.StatusNotFound, CodeNotFound, "session not found")
			return
		}
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}
	dto := SessionRowToDTO(*row)
	dto.AttachedClients = s.Service.AttachedClients(id)
	writeJSON(w, http.StatusOK, dto)
}

func (s *Server) handleStopSession(w http.ResponseWriter, _ *http.Request, id string) {
	if err := s.Service.StopSession(id); err != nil {
		if errors.Is(err, runtime.ErrSessionNotRunning) {
			writeError(w, http.StatusNotFound, CodeNotFound, "session not running")
			return
		}
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleWaitSession(w http.ResponseWriter, r *http.Request, id string) {
	code, err := s.Service.WaitSession(r.Context(), id)
	if err != nil {
		if errors.Is(err, runtime.ErrSessionNotRunning) {
			writeError(w, http.StatusNotFound, CodeNotFound, "session not running")
			return
		}
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, WaitResponse{ExitCode: code})
}
