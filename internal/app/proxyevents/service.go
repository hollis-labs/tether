package proxyevents

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/hollis-labs/tether/internal/callcontext"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/store"
)

// ProxyEventDTO is the on-the-wire shape for a proxy event row.
type ProxyEventDTO struct {
	Attribution      callcontext.Snapshot `json:"attribution"`
	ClaimedSessionID string               `json:"claimed_session_id,omitempty"`
	ID               int64                `json:"id"`
	SessionID        string               `json:"session_id,omitempty"`
	Server           string               `json:"server"`
	ToolName         string               `json:"tool_name"`
	ArgsSchemaFP     string               `json:"args_schema_fp,omitempty"`
	DurationMs       int64                `json:"duration_ms"`
	OK               bool                 `json:"ok"`
	Error            string               `json:"error,omitempty"`
	Timestamp        string               `json:"timestamp"`
}

// ProxyEventListResponse is the envelope returned by GET /proxy/events.
type ProxyEventListResponse struct {
	Events []ProxyEventDTO `json:"events"`
	Count  int             `json:"count"`
}

// ProxyEventIngestRequest is the body accepted by POST /proxy/events.
type ProxyEventIngestRequest struct {
	// Attribution is computed by the daemon, never accepted from JSON.
	Attribution      callcontext.Snapshot `json:"-"`
	ClaimedSessionID string               `json:"claimed_session_id,omitempty"`
	SessionID        string               `json:"session_id"`
	Server           string               `json:"server"`
	ToolName         string               `json:"tool_name"`
	ArgsSchemaFP     string               `json:"args_schema_fp"`
	DurationMs       int64                `json:"duration_ms"`
	OK               bool                 `json:"ok"`
	Error            string               `json:"error"`
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
	// daemon-only `tether mcp` Tether plants in an agent) sets it. A non-empty
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
	MaxIDBytes = 256
	MaxFPBytes = 64
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

// Record is the application query result. Field names intentionally preserve
// the existing proxy MCP tools' legacy JSON shape, independently of storage.
type Record struct {
	ID               int64
	SessionID        string
	Server           string
	ToolName         string
	ArgsSchemaFP     string
	DurationMs       int64
	OK               bool
	Error            string
	Timestamp        time.Time
	Attribution      callcontext.Snapshot
	ClaimedSessionID string
}

// Query is decoded by each transport, preserving its existing limit and parse rules.
type Query struct {
	ServerID   string
	ToolName   string
	SessionID  string
	Limit      int
	Since      time.Time
	ErrorsOnly bool
}

// CallQuery is a decoded MCP query. The application owns limit policy and
// strict timestamp validation; HTTP keeps its existing forgiving decoding.
type CallQuery struct {
	ServerID   string
	ToolName   string
	SessionID  string
	Limit      int
	Since      string
	ErrorsOnly bool
}

// QueryToolCalls retains the proxy tool's default of 50 records.
func QueryToolCalls(rows Querier, q CallQuery) ([]Record, error) { return queryCalls(rows, q, 50) }

// QueryProxyCalls retains the observation tool's default of 100 records.
func QueryProxyCalls(rows Querier, q CallQuery) ([]Record, error) { return queryCalls(rows, q, 100) }
func queryCalls(rows Querier, q CallQuery, defaultLimit int) ([]Record, error) {
	limit := q.Limit
	if limit > 500 {
		limit = 500
	}
	if limit < 1 {
		limit = defaultLimit
	}
	f := Query{ServerID: q.ServerID, ToolName: q.ToolName, SessionID: q.SessionID, Limit: limit, ErrorsOnly: q.ErrorsOnly}
	if q.Since != "" {
		stamp, err := time.Parse(time.RFC3339, q.Since)
		if err != nil {
			return nil, &Error{Code: "invalid_request", Message: "since must be an RFC3339 timestamp: " + err.Error()}
		}
		f.Since = stamp
	}
	return QueryRecords(rows, f)
}

type Querier interface {
	QueryProxyEvents(store.ProxyEventFilter) ([]store.ProxyEvent, error)
}
type Store interface {
	Querier
	AppendProxyEvent(store.ProxyEvent) error
}

type Service struct {
	store         Store
	bus           events.Bus
	sessionExists func(string) error
}

func New(rows Store, bus events.Bus, sessionExists func(string) error) *Service {
	return &Service{store: rows, bus: bus, sessionExists: sessionExists}
}

// Error carries domain failure codes; transports retain their own error envelopes.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

// QueryRecords explicitly translates storage rows, preserving nil versus empty.
func QueryRecords(rows Querier, q Query) ([]Record, error) {
	evs, err := rows.QueryProxyEvents(store.ProxyEventFilter{ServerID: q.ServerID, ToolName: q.ToolName, SessionID: q.SessionID, Limit: q.Limit, Since: q.Since, ErrorsOnly: q.ErrorsOnly})
	if err != nil {
		return nil, err
	}
	var out []Record
	if evs != nil {
		out = make([]Record, 0, len(evs))
	}
	for _, ev := range evs {
		out = append(out, Record{ID: ev.ID, SessionID: ev.SessionID, Server: ev.Server, ToolName: ev.ToolName, ArgsSchemaFP: ev.ArgsSchemaFP, DurationMs: ev.DurationMs, OK: ev.OK, Error: ev.Error, Timestamp: ev.Timestamp, Attribution: ev.Attribution, ClaimedSessionID: ev.ClaimedSessionID})
	}
	return out, nil
}

// Ingest validates before resolving trusted attribution. resolve is provided by
// the authenticated boundary, never by a JSON/body session selector.
func (s *Service) Ingest(ctx context.Context, req ProxyEventIngestRequest, resolve func() callcontext.Snapshot) error {
	if msg := validateIngest(&req); msg != "" {
		return &Error{Code: "bad_request", Message: msg}
	}
	req.Attribution = resolve()
	if req.SessionID != "" && (!req.Attribution.Verified || req.SessionID != req.Attribution.SessionID) {
		req.ClaimedSessionID = req.SessionID
	}
	if req.Attribution.PrincipalID != "" {
		// A verified operator/service is not a session. Its body claim cannot
		// select somebody else's session, even when observe accepts the call.
		req.SessionID = req.Attribution.SessionID
	}
	if req.Publish {
		if s.bus == nil {
			return &Error{Code: "not_found", Message: "event bus not configured"}
		}
		// A published record lands in the event log and fans out to its
		// subscribers, so it may only name a session that exists: a forged
		// session id would otherwise put events into a session that never
		// ran. An empty session id is a call the proxy could not attribute
		// (see CW-20260912-0074) and goes on the daemon scope.
		if req.SessionID != "" {
			if s.sessionExists == nil {
				return &Error{Code: "not_found", Message: "session lookup not configured"}
			}
			if err := s.sessionExists(req.SessionID); err != nil {
				if errors.Is(err, store.ErrSessionNotFound) {
					return &Error{Code: "bad_request", Message: "session_id does not name a session"}
				}
				return &Error{Code: "internal_error", Message: "look up session: " + err.Error()}
			}
		}
	}
	// Server may be empty for native router tools — store it as-is.

	// The daemon stamps the time of every record. The caller's clock is not
	// recorded, so a record cannot be back-dated or forward-dated.
	ts := time.Now().UTC()

	if req.Phase != ProxyEventPhaseStart {
		ev := store.ProxyEvent{
			Attribution:      req.Attribution,
			ClaimedSessionID: req.ClaimedSessionID,
			SessionID:        req.SessionID,
			Server:           req.Server,
			ToolName:         req.ToolName,
			ArgsSchemaFP:     req.ArgsSchemaFP,
			DurationMs:       req.DurationMs,
			OK:               req.OK,
			Error:            req.Error,
			Timestamp:        ts,
		}
		if err := s.store.AppendProxyEvent(ev); err != nil {
			return &Error{Code: "internal_error", Message: "persist proxy event: " + err.Error()}
		}
	}
	if req.Publish {
		if err := s.publishToolCallEvent(ctx, req); err != nil {
			return &Error{Code: "internal_error", Message: "publish tool call event: " + err.Error()}
		}
	}
	return nil
}

// validateIngest checks a POST /proxy/events body's shape and
// truncates an over-long error. It returns the reason a body is refused, or
// "".
func validateIngest(req *ProxyEventIngestRequest) string {
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
		{"tool_name", req.ToolName, MaxIDBytes},
		{"server", req.Server, MaxIDBytes},
		{"session_id", req.SessionID, MaxIDBytes},
		{"claimed_session_id", req.ClaimedSessionID, MaxIDBytes},
		{"args_schema_fp", req.ArgsSchemaFP, MaxFPBytes},
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
func (s *Service) publishToolCallEvent(ctx context.Context, req ProxyEventIngestRequest) error {
	kind := events.EventTypeToolCallEnd
	tce := events.ToolCallEvent{
		Attribution:      req.Attribution,
		ClaimedSessionID: req.ClaimedSessionID,
		SessionID:        req.SessionID,
		ToolName:         req.ToolName,
		Server:           req.Server,
		ArgsSchemaFP:     req.ArgsSchemaFP,
		DurationMs:       req.DurationMs,
		OK:               req.OK,
		Error:            req.Error,
		Timestamp:        time.Now().UTC(),
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
	return s.bus.Publish(ctx, events.Event{
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

func ToDTO(ev Record) ProxyEventDTO {
	return ProxyEventDTO{
		Attribution:      ev.Attribution,
		ClaimedSessionID: ev.ClaimedSessionID,
		ID:               ev.ID,
		SessionID:        ev.SessionID,
		Server:           ev.Server,
		ToolName:         ev.ToolName,
		ArgsSchemaFP:     ev.ArgsSchemaFP,
		DurationMs:       ev.DurationMs,
		OK:               ev.OK,
		Error:            ev.Error,
		Timestamp:        ev.Timestamp.UTC().Format(time.RFC3339Nano),
	}
}
