package environmentstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/store"
)

// IdentityHealth exposes only durable coordinates/counters, never errors or
// event content. A missing progress block means its read was unavailable.
type IdentityHealth struct {
	Ready    bool                         `json:"ready"`
	Progress *store.EventIdentityProgress `json:"progress,omitempty"`
}

// NewStored is the explicit daemon cutover branch. New remains the legacy
// compatibility constructor. No request ever falls back to synthesized IDs.
func NewStored(ctx context.Context, environmentID string, db *store.Store, bus events.Bus) (*Server, error) {
	p, err := db.EventIdentityProgress(ctx)
	if err != nil {
		return nil, err
	}
	if p.EnvironmentID != environmentID {
		return nil, store.ErrEventIdentityMismatch
	}
	s := New(environmentID, db, bus)
	s.StoredIDs = &StoredIDProjection{Cutover: p.Cutover}
	s.identityProgress = db.EventIdentityProgress
	return s, nil
}

func (s *Server) identityReadiness(ctx context.Context) (store.EventIdentityProgress, error) {
	p, err := s.identityProgress(ctx)
	if err != nil {
		return p, err
	}
	if s.StoredIDs == nil {
		return p, fmt.Errorf("stored projection is not configured")
	}
	if p.EnvironmentID != s.EnvironmentID || p.Cutover != s.StoredIDs.Cutover {
		return p, store.ErrEventIdentityMismatch
	}
	if p.Remaining != 0 || p.NextLegacyID != 0 {
		return p, ErrBackfillInProgress
	}
	return p, nil
}

func (s *Server) EventIdentityHealth(ctx context.Context) *IdentityHealth {
	if s.identityProgress == nil {
		return nil
	}
	p, err := s.identityReadiness(ctx)
	h := &IdentityHealth{Ready: err == nil}
	if err == nil || errors.Is(err, ErrBackfillInProgress) {
		h.Progress = &p
	}
	return h
}

func (s *Server) checkIdentityReady(ctx context.Context) error {
	if s.identityProgress == nil {
		return nil
	}
	_, err := s.identityReadiness(ctx)
	if err != nil && !errors.Is(err, ErrBackfillInProgress) {
		return fmt.Errorf("event identity unavailable: %w", err)
	}
	return err
}

func (s *Server) writeIdentityNotReady(w http.ResponseWriter, err error) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	if errors.Is(err, ErrBackfillInProgress) {
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{"reason": "backfill_in_progress", "retry_after_seconds": 1, "snapshot_required": false})
		return
	}
	w.WriteHeader(http.StatusServiceUnavailable)
	_ = json.NewEncoder(w).Encode(map[string]string{"reason": "event_identity_unavailable"})
}
