package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/chrispian/agent-mux/internal/store"
)

// ProxyEventStore is the narrow store contract for the /proxy/events handler.
// *store.Store satisfies this interface.
type ProxyEventStore interface {
	AppendProxyEvent(ev store.ProxyEvent) error
	QueryProxyEvents(f store.ProxyEventFilter) ([]store.ProxyEvent, error)
}

// ProxyEventDTO is the on-the-wire shape for a proxy event row.
type ProxyEventDTO struct {
	ID           int64  `json:"id"`
	SessionID    string `json:"session_id,omitempty"`
	Server       string `json:"server"`
	ToolName     string `json:"tool_name"`
	ArgsSchemaFP string `json:"args_schema_fp,omitempty"`
	DurationMs   int64  `json:"duration_ms"`
	OK           bool   `json:"ok"`
	Error        string `json:"error,omitempty"`
	Timestamp    string `json:"timestamp"`
}

// ProxyEventListResponse is the envelope returned by GET /proxy/events.
type ProxyEventListResponse struct {
	Events []ProxyEventDTO `json:"events"`
	Count  int             `json:"count"`
}

// ProxyEventIngestRequest is the body accepted by POST /proxy/events.
type ProxyEventIngestRequest struct {
	SessionID    string `json:"session_id"`
	Server       string `json:"server"`
	ToolName     string `json:"tool_name"`
	ArgsSchemaFP string `json:"args_schema_fp"`
	DurationMs   int64  `json:"duration_ms"`
	OK           bool   `json:"ok"`
	Error        string `json:"error"`
	Timestamp    string `json:"timestamp"` // RFC3339; defaults to now if empty
}

func (s *Server) registerProxyEventRoutes(mux *http.ServeMux) {
	if s.ProxyEvents == nil {
		return
	}
	mux.HandleFunc("/proxy/events", s.handleProxyEvents)
}

func (s *Server) handleProxyEvents(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleListProxyEvents(w, r)
	case http.MethodPost:
		s.handleIngestProxyEvent(w, r)
	default:
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
	}
}

// handleListProxyEvents serves GET /proxy/events.
//
// Query params (all optional):
//
//	?server=hadron          – filter by exact server ID
//	?tool_name=hadron_      – filter by tool name prefix
//	?session_id=abc         – filter by session ID
//	?limit=50               – max results (default 100, max 500)
//	?since=2026-01-01T00:00:00Z – RFC3339 lower bound on timestamp
//	?errors_only=true       – only return failed calls
func (s *Server) handleListProxyEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	f := store.ProxyEventFilter{
		ServerID:  q.Get("server"),
		ToolName:  q.Get("tool_name"),
		SessionID: q.Get("session_id"),
	}

	if lim := q.Get("limit"); lim != "" {
		if n, err := strconv.Atoi(lim); err == nil {
			f.Limit = n
		}
	}
	if since := q.Get("since"); since != "" {
		if t, err := time.Parse(time.RFC3339, since); err == nil {
			f.Since = t
		}
	}
	if q.Get("errors_only") == "true" {
		f.ErrorsOnly = true
	}

	evs, err := s.ProxyEvents.QueryProxyEvents(f)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, "query proxy events: "+err.Error())
		return
	}

	dtos := make([]ProxyEventDTO, len(evs))
	for i, ev := range evs {
		dtos[i] = proxyEventToDTO(ev)
	}
	writeJSON(w, http.StatusOK, ProxyEventListResponse{Events: dtos, Count: len(dtos)})
}

// handleIngestProxyEvent serves POST /proxy/events. Called by the mcp
// subprocess to persist a tool_call_end event into the daemon's database so
// the TUI can read it via GET /proxy/events.
func (s *Server) handleIngestProxyEvent(w http.ResponseWriter, r *http.Request) {
	var req ProxyEventIngestRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body: "+err.Error())
		return
	}
	if req.ToolName == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "tool_name is required")
		return
	}
	// Server may be empty for native mux tools — store it as-is.

	ts := time.Now().UTC()
	if req.Timestamp != "" {
		if t, err := time.Parse(time.RFC3339Nano, req.Timestamp); err == nil {
			ts = t
		} else if t, err := time.Parse(time.RFC3339, req.Timestamp); err == nil {
			ts = t
		}
	}

	ev := store.ProxyEvent{
		SessionID:    req.SessionID,
		Server:       req.Server,
		ToolName:     req.ToolName,
		ArgsSchemaFP: req.ArgsSchemaFP,
		DurationMs:   req.DurationMs,
		OK:           req.OK,
		Error:        req.Error,
		Timestamp:    ts,
	}
	if err := s.ProxyEvents.AppendProxyEvent(ev); err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, "persist proxy event: "+err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true})
}

func proxyEventToDTO(ev store.ProxyEvent) ProxyEventDTO {
	return ProxyEventDTO{
		ID:           ev.ID,
		SessionID:    ev.SessionID,
		Server:       ev.Server,
		ToolName:     ev.ToolName,
		ArgsSchemaFP: ev.ArgsSchemaFP,
		DurationMs:   ev.DurationMs,
		OK:           ev.OK,
		Error:        ev.Error,
		Timestamp:    ev.Timestamp.UTC().Format(time.RFC3339Nano),
	}
}
