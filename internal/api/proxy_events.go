package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/store"
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
	// Timestamp is accepted for compatibility and ignored: the daemon stamps
	// the time of every record itself, so a caller cannot back-date one.
	Timestamp string `json:"timestamp,omitempty"`

	// Phase is the call's phase: ProxyEventPhaseEnd (the default, when
	// empty) records a finished call in proxy_events; ProxyEventPhaseStart
	// records nothing there and is only meaningful with Publish.
	Phase string `json:"phase,omitempty"`
	// Publish asks the daemon to also publish the call on its event bus as
	// a tool_call_start / tool_call_end event, which the bus persists to the
	// events table. A proxy that cannot write the event log itself (the
	// daemon-only `mux mcp` Tether plants in an agent) sets it. A non-empty
	// SessionID must then name an existing session. Every field but the time
	// is the caller's assertion.
	Publish bool `json:"publish,omitempty"`
}

// Phases of a proxied tool call accepted by POST /proxy/events.
const (
	ProxyEventPhaseStart = "start"
	ProxyEventPhaseEnd   = "end"
)

// Size limits POST /proxy/events applies. A longer identifier is refused;
// a longer error is truncated, so one verbose upstream error does not lose
// the record of its call. A body over maxProxyEventBodyBytes is refused whole,
// so a caller must truncate the error itself (TruncateProxyEventError) before
// sending: an upstream error can be far larger than the body limit, and a
// refused end record leaves an orphan start.
const (
	maxProxyEventBodyBytes = 64 << 10
	maxProxyEventIDBytes   = 256
	maxProxyEventFPBytes   = 64
	// MaxProxyEventErrorBytes is the longest error text a record carries.
	MaxProxyEventErrorBytes = 4 << 10
)

// proxyEventErrorTruncated ends an error text TruncateProxyEventError cut.
const proxyEventErrorTruncated = "…[truncated]"

// TruncateProxyEventError returns s unchanged when it fits a record, and
// otherwise its start cut on a rune boundary, followed by a marker, within
// MaxProxyEventErrorBytes in all. The daemon applies it to what it receives;
// a proxy applies it before sending, so a very large error does not push the
// body over the limit.
func TruncateProxyEventError(s string) string {
	if len(s) <= MaxProxyEventErrorBytes {
		return s
	}
	return truncateUTF8(s, MaxProxyEventErrorBytes-len(proxyEventErrorTruncated)) + proxyEventErrorTruncated
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
	r.Body = http.MaxBytesReader(w, r.Body, maxProxyEventBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body: "+err.Error())
		return
	}
	if msg := validateProxyEventIngest(&req); msg != "" {
		writeError(w, http.StatusBadRequest, "bad_request", msg)
		return
	}
	if req.Publish {
		if s.Bus == nil {
			writeError(w, http.StatusNotFound, CodeNotFound, "event bus not configured")
			return
		}
		// A published record lands in the event log and fans out to its
		// subscribers, so it may only name a session that exists: a forged
		// session id would otherwise put events into a session that never
		// ran. An empty session id is a call the proxy could not attribute
		// (see CW-20260912-0074) and goes on the daemon scope.
		if req.SessionID != "" {
			if s.Service == nil {
				writeError(w, http.StatusNotFound, CodeNotFound, "session lookup not configured")
				return
			}
			if _, err := s.Service.GetSession(req.SessionID); err != nil {
				if errors.Is(err, store.ErrSessionNotFound) {
					writeError(w, http.StatusBadRequest, "bad_request", "session_id does not name a session")
					return
				}
				writeError(w, http.StatusInternalServerError, CodeInternalError, "look up session: "+err.Error())
				return
			}
		}
	}
	// Server may be empty for native mux tools — store it as-is.

	// The daemon stamps the time of every record. The caller's clock is not
	// recorded, so a record cannot be back-dated or forward-dated.
	ts := time.Now().UTC()

	if req.Phase != ProxyEventPhaseStart {
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
	}
	if req.Publish {
		if err := s.publishToolCallEvent(r, req); err != nil {
			writeError(w, http.StatusInternalServerError, CodeInternalError, "publish tool call event: "+err.Error())
			return
		}
	}
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true})
}

// validateProxyEventIngest checks a POST /proxy/events body's shape and
// truncates an over-long error. It returns the reason a body is refused, or
// "".
func validateProxyEventIngest(req *ProxyEventIngestRequest) string {
	switch req.Phase {
	case "", ProxyEventPhaseEnd, ProxyEventPhaseStart:
	default:
		return "phase must be \"start\" or \"end\""
	}
	if req.Phase == ProxyEventPhaseStart && !req.Publish {
		return "a start record is only published: set publish"
	}
	if req.ToolName == "" {
		return "tool_name is required"
	}
	for _, f := range []struct {
		name, value string
		max         int
	}{
		{"tool_name", req.ToolName, maxProxyEventIDBytes},
		{"server", req.Server, maxProxyEventIDBytes},
		{"session_id", req.SessionID, maxProxyEventIDBytes},
		{"args_schema_fp", req.ArgsSchemaFP, maxProxyEventFPBytes},
	} {
		if len(f.value) > f.max {
			return f.name + " is longer than " + strconv.Itoa(f.max) + " bytes"
		}
	}
	if req.DurationMs < 0 {
		return "duration_ms must not be negative"
	}
	req.Error = TruncateProxyEventError(req.Error)
	return ""
}

// publishToolCallEvent publishes req on the daemon's bus in the shape
// LoggingMiddleware publishes a proxied call in-process, stamped with the
// daemon's own time.
func (s *Server) publishToolCallEvent(r *http.Request, req ProxyEventIngestRequest) error {
	kind := events.EventTypeToolCallEnd
	tce := events.ToolCallEvent{
		SessionID:    req.SessionID,
		ToolName:     req.ToolName,
		Server:       req.Server,
		ArgsSchemaFP: req.ArgsSchemaFP,
		DurationMs:   req.DurationMs,
		OK:           req.OK,
		Error:        req.Error,
		Timestamp:    time.Now().UTC(),
	}
	if req.Phase == ProxyEventPhaseStart {
		kind = events.EventTypeToolCallStart
		tce.DurationMs, tce.OK, tce.Error = 0, false, ""
	}
	raw, err := json.Marshal(tce)
	if err != nil {
		return err
	}
	scope := events.ScopeSession
	if tce.SessionID == "" {
		scope = events.ScopeDaemon
	}
	return s.Bus.Publish(r.Context(), events.Event{
		Scope:       scope,
		SessionID:   tce.SessionID,
		Kind:        kind,
		PayloadJSON: string(raw),
	})
}

// truncateUTF8 cuts s to at most n bytes without splitting a rune.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
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
