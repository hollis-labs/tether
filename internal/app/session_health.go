package app

import (
	"context"
	"time"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/environmentstream"
	"github.com/hollis-labs/tether/internal/store"
)

// SessionHealthObservations reads the existing request reducer and the reaper's
// activity sources. It never treats bookkeeping timestamps as runtime activity.
func (s *Service) SessionHealthObservations(ctx context.Context, id string) (api.SessionHealthObservations, error) {
	var out api.SessionHealthObservations
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	base, err := s.Store.EnvironmentSnapshot(ctx, id)
	if err != nil {
		return out, err
	}
	state, ok := environmentstream.SessionFromSnapshot(base, id)
	if !ok {
		return out, store.ErrSessionNotFound
	}
	if state.ApprovalKnown {
		out.PendingApproval = &state.PendingApproval
	}
	if state.QuestionKnown {
		out.PendingQuestion = &state.PendingQuestion
	}
	activity := store.ReaperSession{SessionRow: store.SessionRow{ID: id}}
	if err := s.Store.ReaperTimes(ctx, &activity); err != nil {
		return api.SessionHealthObservations{}, err
	}
	last := activity.LastActivity
	if live := s.SessionLastActivity(id); live.After(last) {
		last = live
	}
	if !last.IsZero() {
		last = last.UTC()
		out.LastActivityAt = &last
	}
	return out, nil
}
