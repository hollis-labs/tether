//go:build !windows

package app

import (
	"context"
	"database/sql"
	"errors"

	"github.com/hollis-labs/tether/internal/store"
)

// Preserved old placement metadata is accounting history, never a controller
// for the replacement. Lookup failure cannot authorize operational dispatch.
func (s *Service) currentShimExecution(ctx context.Context, id string) error {
	if s.Store == nil {
		return store.ErrSessionReplacementUnavailable
	}
	_, err := s.Store.SessionReplacement(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	return store.ErrSessionReplacementUnavailable
}
