package daemon

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/chrispian/agent-mux/internal/runtime"
)

// registerSessionRoutes wires /sessions handlers onto mux. Route matching is
// deliberately hand-rolled to avoid pulling in a third-party router for
// three static paths + two parameterised ones.
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
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) handleSessionsItem(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/sessions/")
	if rest == "" {
		writeError(w, http.StatusNotFound, "session id required")
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
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		s.handleGetSession(w, r, id)
	case "stop":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		s.handleStopSession(w, r, id)
	case "wait":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		s.handleWaitSession(w, r, id)
	default:
		writeError(w, http.StatusNotFound, "unknown action "+action)
	}
}

func (s *Server) handleLaunch(w http.ResponseWriter, r *http.Request) {
	var req LaunchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if req.Launch == "" {
		writeError(w, http.StatusBadRequest, "launch id required")
		return
	}
	res, err := s.Service.Launch(req.Launch)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, LaunchResponse{
		ID:        res.SessionID,
		Workspace: res.Workspace,
		Log:       res.LogPath,
	})
}

func (s *Server) handleListSessions(w http.ResponseWriter, _ *http.Request) {
	rows, err := s.Service.ListSessions()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]SessionDTO, 0, len(rows))
	for _, row := range rows {
		out = append(out, SessionRowToDTO(row))
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
			writeError(w, http.StatusNotFound, "session not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, SessionRowToDTO(*row))
}

func (s *Server) handleStopSession(w http.ResponseWriter, _ *http.Request, id string) {
	if err := s.Service.StopSession(id); err != nil {
		if errors.Is(err, runtime.ErrSessionNotRunning) {
			writeError(w, http.StatusNotFound, "session not running")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleWaitSession(w http.ResponseWriter, r *http.Request, id string) {
	code, err := s.Service.WaitSession(r.Context(), id)
	if err != nil {
		if errors.Is(err, runtime.ErrSessionNotRunning) {
			writeError(w, http.StatusNotFound, "session not running")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, WaitResponse{ExitCode: code})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, ErrorResponse{Error: msg})
}
