package environmentstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/store"
)

type Store interface {
	EnvironmentEvents(context.Context, int64, int) (store.EnvironmentWindow, error)
	EnvironmentSnapshot(context.Context, string) (store.EnvironmentSnapshot, error)
}

// Server uses live notifications as hints only. Every delivered event is read
// durably, so notification drops cannot become silent gaps in the HTTP stream.
type Server struct {
	EnvironmentID string
	Store         Store
	Bus           events.Bus
	MaxCatchUp    int
	PollInterval  time.Duration
	WriteTimeout  time.Duration
	// StoredIDs is nil in the compatibility constructor. NewStored selects
	// it explicitly, with readiness checked before any response data.
	StoredIDs *StoredIDProjection
	// LocalIdentityMode is the identity mode of the local listener this server
	// is mounted on; it decides whether a request without a principal is the
	// trusted local operator (TrustedLocal).
	LocalIdentityMode identity.Mode
	identityProgress  func(context.Context) (store.EventIdentityProgress, error)
}

func New(environmentID string, db *store.Store, bus events.Bus) *Server {
	return &Server{EnvironmentID: environmentID, Store: db, Bus: bus, MaxCatchUp: 2048, PollInterval: time.Second, WriteTimeout: 15 * time.Second}
}

func (s *Server) Snapshot(w http.ResponseWriter, r *http.Request, sessionID string) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if err := s.checkIdentityReady(r.Context()); err != nil {
		s.writeIdentityNotReady(w, err)
		return
	}
	base, err := s.Store.EnvironmentSnapshot(r.Context(), sessionID)
	if err != nil {
		writeReadFailure(w, "snapshot", err)
		return
	}
	if sessionID != "" && len(base.Sessions) == 0 {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(snapshot(s.EnvironmentID, base))
}

// readFailureClass names why a durable read failed, for the answer and the log.
// "busy" and "timeout" are transient (SQLite could not take its lock in time, or
// the caller's deadline or connection ended the read): the caller retries.
// Anything else is an internal failure.
func readFailureClass(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "database is locked") || strings.Contains(msg, "sqlite_busy") || strings.Contains(msg, "database table is locked") {
		return "busy"
	}
	return "internal"
}

// writeReadFailure answers a failed durable read. A transient failure is a 503
// with Retry-After and a body that says to retry; an internal one stays a 500
// with the generic body. The real error is logged (the class and the driver's
// message, which carries no request values), never sent to the caller.
func writeReadFailure(w http.ResponseWriter, what string, err error) {
	class := readFailureClass(err)
	detail := err.Error()
	if len(detail) > 200 {
		detail = detail[:200]
	}
	log.Printf("environment %s read failed: class=%s err=%q", what, class, detail)
	switch class {
	case "busy", "timeout", "canceled":
		w.Header().Set("Retry-After", "2")
		http.Error(w, what+" unavailable: temporarily busy, retry", http.StatusServiceUnavailable)
	default:
		http.Error(w, what+" unavailable", http.StatusInternalServerError)
	}
}

type Gap struct {
	EnvironmentID     string `json:"environment_id"`
	Reason            string `json:"reason"`
	EarliestAvailable int64  `json:"earliest_available"`
	HighWaterSeq      int64  `json:"high_water_seq"`
	SnapshotRequired  bool   `json:"snapshot_required"`
	RetryAfterSeconds int    `json:"retry_after_seconds,omitempty"`
}

func (s *Server) checkWindowIDs(window store.EnvironmentWindow, after int64) error {
	if s.StoredIDs == nil || window.GapReason != "" {
		return nil
	}
	var pending error
	for _, e := range window.Events {
		if e.Seq > after {
			if err := s.StoredIDs.checkID(e); err != nil {
				if errors.Is(err, ErrBackfillInProgress) {
					pending = err
					continue
				}
				return err
			}
		}
	}
	return pending
}

// Stream registers live delivery BEFORE reading the durable catch-up. It then
// emits a synchronized marker at the read's high-water, drains new windows,
// and ignores duplicate notification sequences. Session filtering is applied
// after checking the complete global window; sync markers advance its cursor.
// Event filters (see Filter) and visibility apply the same way: they decide what
// is written, never what is consumed.
func (s *Server) Stream(w http.ResponseWriter, r *http.Request, sessionID string) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if err := s.checkIdentityReady(r.Context()); err != nil {
		s.writeIdentityNotReady(w, err)
		return
	}
	raw := r.URL.Query().Get("after_seq")
	if raw == "" {
		raw = r.Header.Get("Last-Event-ID")
	}
	if raw == "" {
		http.Error(w, "after_seq required; take a snapshot first", http.StatusBadRequest)
		return
	}
	after, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || after < 0 {
		http.Error(w, "invalid after_seq", http.StatusBadRequest)
		return
	}
	filter, err := ParseFilter(r.URL.Query())
	if err != nil {
		http.Error(w, "invalid filter: "+err.Error(), http.StatusBadRequest)
		return
	}
	principal, hasPrincipal := identity.FromContext(r.Context())
	trustedLocal := TrustedLocal(s.LocalIdentityMode, r)
	liveBus, ok := s.Bus.(events.LiveSubscriber)
	if !ok {
		http.Error(w, "live stream unavailable", http.StatusServiceUnavailable)
		return
	}
	if _, ok = w.(http.Flusher); !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	if sessionID != "" {
		base, readErr := s.Store.EnvironmentSnapshot(r.Context(), sessionID)
		if readErr != nil {
			writeReadFailure(w, "snapshot", readErr)
			return
		}
		if len(base.Sessions) == 0 {
			http.NotFound(w, r)
			return
		}
	}
	live, cancel, err := liveBus.SubscribeLive(r.Context(), events.Filter{})
	if err != nil {
		http.Error(w, "subscribe unavailable", http.StatusServiceUnavailable)
		return
	}
	defer cancel()
	limit := s.MaxCatchUp
	if limit <= 0 {
		limit = 2048
	}
	window, err := s.Store.EnvironmentEvents(r.Context(), after, limit)
	if err != nil {
		writeReadFailure(w, "event tail", err)
		return
	}
	// Validate the whole global window before session filtering or committing
	// SSE headers. A filtered-out legacy row must not advance the resume cursor.
	if err := s.checkWindowIDs(window, after); err != nil {
		if errors.Is(err, ErrBackfillInProgress) {
			w.Header().Set("Retry-After", "1")
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(Gap{EnvironmentID: s.EnvironmentID, Reason: ErrBackfillInProgress.Error(), EarliestAvailable: window.EarliestAvailable, HighWaterSeq: window.HighWater, RetryAfterSeconds: 1})
		} else {
			http.Error(w, "event_id_invariant", http.StatusInternalServerError)
		}
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	control := http.NewResponseController(w)
	emit := func(kind string, id int64, data any) error {
		if s.WriteTimeout > 0 {
			_ = control.SetWriteDeadline(time.Now().Add(s.WriteTimeout))
		}
		body, marshalErr := json.Marshal(data)
		if marshalErr != nil {
			return marshalErr
		}
		if _, writeErr := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", id, kind, body); writeErr != nil {
			return writeErr
		}
		return control.Flush()
	}
	gap := func(reason string, window store.EnvironmentWindow) {
		// A gap never advances Last-Event-ID to the head: only a new snapshot
		// authorizes discarding the missing history.
		g := Gap{EnvironmentID: s.EnvironmentID, Reason: reason, EarliestAvailable: window.EarliestAvailable, HighWaterSeq: window.HighWater, SnapshotRequired: true}
		if reason == ErrBackfillInProgress.Error() {
			g.SnapshotRequired = false
			g.RetryAfterSeconds = 1
		}
		_ = emit("gap", after, g)
	}
	drain := func(window store.EnvironmentWindow) bool {
		if window.GapReason != "" {
			gap(window.GapReason, window)
			return false
		}
		if err := s.checkWindowIDs(window, after); err != nil {
			reason := "event_id_invariant"
			if errors.Is(err, ErrBackfillInProgress) {
				reason = ErrBackfillInProgress.Error()
			}
			gap(reason, window)
			return false
		}
		for _, stored := range window.Events {
			if stored.Seq <= after {
				continue
			}
			if sessionID == "" || stored.SessionID == sessionID {
				mapEvent := Map
				if s.StoredIDs != nil {
					mapEvent = s.StoredIDs.MapStored
				}
				e, mappingErr := mapEvent(s.EnvironmentID, stored)
				if mappingErr != nil {
					gap("restart", window)
					return false
				}
				// A filtered-out event is consumed like any other: the cursor
				// below still advances past it.
				if filter.Match(e) && Visible(e.Visibility, principal, hasPrincipal, trustedLocal) {
					if emit(e.Kind, e.Seq, e) != nil {
						return false
					}
				}
			}
			after = stored.Seq
		}
		after = window.HighWater
		return true
	}
	if !drain(window) {
		return
	}
	sync := func() error {
		return emit("synchronized", after, struct {
			EnvironmentID string `json:"environment_id"`
			Seq           int64  `json:"seq"`
		}{s.EnvironmentID, after})
	}
	if sync() != nil {
		return
	}
	interval := s.PollInterval
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case _, open := <-live:
			if !open {
				gap("restart", window)
				return
			}
		case <-ticker.C:
		}
		window, err = s.Store.EnvironmentEvents(r.Context(), after, limit)
		if err != nil {
			gap("restart", window)
			return
		}
		if !drain(window) {
			return
		}
		if sync() != nil {
			return
		}
	}
}
