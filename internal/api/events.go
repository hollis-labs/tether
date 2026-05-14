package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hollis-labs/tether/internal/events"
)

// EventsStore is the narrow read contract the /sessions/{id}/events
// handler depends on. Bus subscription covers the live/replay stream
// separately via events.Bus.
type EventsStore interface {
	ListEventsBySession(sessionID string, limit int, cursor int64) ([]events.Event, error)
}

// EventDTO is the on-the-wire shape for a historical event row.
type EventDTO struct {
	Seq         int64  `json:"seq"`
	At          string `json:"at"`
	Scope       string `json:"scope"`
	SessionID   string `json:"session_id,omitempty"`
	Kind        string `json:"kind"`
	PayloadJSON string `json:"payload_json,omitempty"`
}

type EventListResponse struct {
	Events     []EventDTO `json:"events"`
	NextCursor int64      `json:"next_cursor,omitempty"`
}

func eventToDTO(e events.Event) EventDTO {
	return EventDTO{
		Seq:         e.Seq,
		At:          e.At.UTC().Format(time.RFC3339Nano),
		Scope:       e.Scope,
		SessionID:   e.SessionID,
		Kind:        e.Kind,
		PayloadJSON: e.PayloadJSON,
	}
}

func (s *Server) registerEventRoutes(mux *http.ServeMux) {
	if s.Bus != nil {
		mux.HandleFunc("/events/stream", s.handleEventsStream)
	}
	// Per-session history mounts under the sessions dispatcher's
	// "events" action case; see handleSessionsItem.
}

// handleEventsStream services GET /events/stream as an SSE stream.
// Query params:
//   - ?since_seq=N : replay bus events with seq > N before live
//   - ?scope=X     : repeatable; one of session|daemon|broker
//   - ?session_id= : filter to a single session's events
//
// SSE framing:
//
//	id: <seq>
//	event: <kind>
//	data: <payload_json or empty>
//	<blank line>
//
// A keep-alive comment (`: ping`) fires every 15s so intermediate
// proxies don't close idle connections.
func (s *Server) handleEventsStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, CodeInternalError, "streaming not supported")
		return
	}

	filter, err := parseEventsFilter(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, err.Error())
		return
	}

	ch, cancel, err := s.Bus.Subscribe(r.Context(), filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}
	defer cancel()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()

	for {
		select {
		case e, open := <-ch:
			if !open {
				return
			}
			writeSSEEvent(w, flusher, e)
		case <-ping.C:
			// Keep-alive comment; SSE convention — lines starting with
			// ":" are ignored by the parser.
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

// writeSSEEvent emits one event as a well-formed SSE block. Errors on
// write abort the stream — the client will reconnect if it wants more.
func writeSSEEvent(w http.ResponseWriter, flusher http.Flusher, e events.Event) {
	var buf strings.Builder
	fmt.Fprintf(&buf, "id: %d\n", e.Seq)
	if e.Kind != "" {
		fmt.Fprintf(&buf, "event: %s\n", e.Kind)
	}
	// Envelope the payload + scope + session_id so subscribers can parse
	// structured data. Raw PayloadJSON is also kept for callers that
	// want the untouched payload.
	data := struct {
		Scope       string `json:"scope"`
		SessionID   string `json:"session_id,omitempty"`
		PayloadJSON string `json:"payload_json,omitempty"`
	}{
		Scope:       e.Scope,
		SessionID:   e.SessionID,
		PayloadJSON: e.PayloadJSON,
	}
	b, _ := json.Marshal(data)
	fmt.Fprintf(&buf, "data: %s\n\n", b)
	if _, err := w.Write([]byte(buf.String())); err != nil {
		return
	}
	flusher.Flush()
}

// parseEventsFilter builds an events.Filter from query params. Rejects
// malformed values with a descriptive error — caller returns 400.
func parseEventsFilter(r *http.Request) (events.Filter, error) {
	q := r.URL.Query()
	f := events.Filter{SessionID: q.Get("session_id")}
	if raw := q.Get("since_seq"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 0 {
			return f, fmt.Errorf("since_seq must be a non-negative integer")
		}
		f.SinceSeq = n
	}
	for _, s := range q["scope"] {
		switch s {
		case events.ScopeSession, events.ScopeDaemon, events.ScopeBroker:
			f.Scopes = append(f.Scopes, s)
		default:
			return f, fmt.Errorf("scope must be one of session/daemon/broker")
		}
	}
	return f, nil
}

// handleSessionEventsList services GET /sessions/{id}/events. Returns
// historical events for the session in descending seq order with
// optional ?limit= and ?cursor= pagination. When the page fills to
// limit, next_cursor is set to the smallest returned seq; callers pass
// that back to continue paging.
func (s *Server) handleSessionEventsList(w http.ResponseWriter, r *http.Request, sessionID string) {
	q := r.URL.Query()
	limit := 0
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, "limit must be a non-negative integer")
			return
		}
		limit = n
	}
	var cursor int64
	if raw := q.Get("cursor"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, "cursor must be a non-negative integer")
			return
		}
		cursor = n
	}

	rows, err := s.EventsStore.ListEventsBySession(sessionID, limit, cursor)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}
	out := make([]EventDTO, 0, len(rows))
	for _, e := range rows {
		out = append(out, eventToDTO(e))
	}
	resp := EventListResponse{Events: out}
	effective := limit
	if effective <= 0 {
		effective = 100
	}
	if len(rows) == effective && len(rows) > 0 {
		resp.NextCursor = rows[len(rows)-1].Seq
	}
	writeJSON(w, http.StatusOK, resp)
}
