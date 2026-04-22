package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/chrispian/agent-mux/internal/runtime"
	"github.com/chrispian/agent-mux/internal/store"
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
	case "resize":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
			return
		}
		s.handleResizeSession(w, r, id)
	case "attach":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
			return
		}
		s.handleAttach(w, r, id)
	case "checkpoint":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
			return
		}
		if s.Checkpoints == nil {
			writeError(w, http.StatusNotFound, CodeNotFound, "checkpoints not configured")
			return
		}
		s.handleCreateCheckpoint(w, r, id)
	case "events":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
			return
		}
		if s.EventsStore == nil {
			writeError(w, http.StatusNotFound, CodeNotFound, "events store not configured")
			return
		}
		s.handleSessionEventsList(w, r, id)
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
	var res LaunchResult
	var err error
	if req.BootPrompt != "" {
		res, err = s.Service.CreateSessionWithBootPrompt(req.Launch, req.BootPrompt)
	} else {
		res, err = s.Service.CreateSession(req.Launch)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, LaunchResponse{
		ID:         res.SessionID,
		Workspace:  res.Workspace,
		Log:        res.LogPath,
		ProviderID: res.ProviderID,
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
		ID:         res.SessionID,
		Workspace:  res.Workspace,
		Log:        res.LogPath,
		ProviderID: res.ProviderID,
	})
}

// handleListSessions services GET /sessions. Supports three query
// params: ?limit= (1..1000, default 100), ?cursor= (RFC3339, returns
// rows strictly older than this timestamp), ?state= (filters to a
// single session state). Malformed params yield 400 so callers know
// to fix them rather than silently paging the full set.
func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	opts, err := parseListSessionsOpts(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, err.Error())
		return
	}

	rows, err := s.Service.ListSessions(opts)
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

	// When we hit the requested page size, more rows may exist older
	// than the last returned row's created_at — hand that back as the
	// next cursor. When we returned fewer than the limit, we know we
	// reached the end and emit no cursor.
	resp := ListSessionsResponse{Sessions: out}
	effectiveLimit := opts.Limit
	if effectiveLimit <= 0 {
		effectiveLimit = 100
	}
	if len(rows) == effectiveLimit && len(rows) > 0 {
		resp.NextCursor = rows[len(rows)-1].CreatedAt
	}
	writeJSON(w, http.StatusOK, resp)
}

// parseListSessionsOpts extracts pagination + filter params from the
// request URL. Returns a typed error message for malformed values; the
// caller maps it to 400.
func parseListSessionsOpts(r *http.Request) (store.ListSessionsOptions, error) {
	q := r.URL.Query()
	opts := store.ListSessionsOptions{
		Cursor: q.Get("cursor"),
		State:  q.Get("state"),
	}
	if s := q.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil {
			return opts, errors.New("invalid limit: must be integer")
		}
		if n < 0 {
			return opts, errors.New("invalid limit: must be >= 0")
		}
		opts.Limit = n
	}
	return opts, nil
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

// handleResizeSession services POST /sessions/{id}/resize. Body is a
// ResizeRequest with rows + cols (both > 0). On success returns 204.
// Typed errors: invalid_request (missing/zero fields), not_found
// (session id unknown OR not currently running — the underlying
// runtime distinguishes only ErrSessionNotRunning).
func (s *Server) handleResizeSession(w http.ResponseWriter, r *http.Request, id string) {
	var req ResizeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "invalid request body: "+err.Error())
		return
	}
	if req.Rows == 0 || req.Cols == 0 {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "rows and cols must be > 0")
		return
	}
	if err := s.Service.ResizeSession(id, req.Rows, req.Cols); err != nil {
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
