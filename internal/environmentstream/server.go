package environmentstream

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/hollis-labs/tether/internal/events"
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
}

func New(environmentID string, db *store.Store, bus events.Bus) *Server {
	return &Server{EnvironmentID: environmentID, Store: db, Bus: bus, MaxCatchUp: 2048, PollInterval: time.Second, WriteTimeout: 15 * time.Second}
}

func (s *Server) Snapshot(w http.ResponseWriter, r *http.Request, sessionID string) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	base, err := s.Store.EnvironmentSnapshot(r.Context(), sessionID)
	if err != nil {
		http.Error(w, "snapshot unavailable", http.StatusInternalServerError)
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

type Gap struct {
	EnvironmentID     string `json:"environment_id"`
	Reason            string `json:"reason"`
	EarliestAvailable int64  `json:"earliest_available"`
	HighWaterSeq      int64  `json:"high_water_seq"`
	SnapshotRequired  bool   `json:"snapshot_required"`
}

// Stream registers live delivery BEFORE reading the durable catch-up. It then
// emits a synchronized marker at the read's high-water, drains new windows,
// and ignores duplicate notification sequences. Session filtering is applied
// after checking the complete global window; sync markers advance its cursor.
func (s *Server) Stream(w http.ResponseWriter, r *http.Request, sessionID string) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
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
			http.Error(w, "snapshot unavailable", http.StatusInternalServerError)
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
		http.Error(w, "event tail unavailable", http.StatusInternalServerError)
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
		_ = emit("gap", after, Gap{s.EnvironmentID, reason, window.EarliestAvailable, window.HighWater, true})
	}
	drain := func(window store.EnvironmentWindow) bool {
		if window.GapReason != "" {
			gap(window.GapReason, window)
			return false
		}
		for _, stored := range window.Events {
			if stored.Seq <= after {
				continue
			}
			if sessionID == "" || stored.SessionID == sessionID {
				e, mappingErr := Map(s.EnvironmentID, stored)
				if mappingErr != nil {
					gap("restart", window)
					return false
				}
				if emit(e.Kind, e.Seq, e) != nil {
					return false
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
