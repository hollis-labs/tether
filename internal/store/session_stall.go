package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/hollis-labs/tether/internal/events"
)

// SessionActivity summarises a session's recent event history for stall
// detection: when it last did anything and how many tool actions the provider
// refused since it last produced turn output.
type SessionActivity struct {
	LastEventAt       time.Time
	DeniedSinceOutput int
}

// SessionActivity reads the durable events table. A session with no events
// returns the zero value.
func (s *Store) SessionActivity(ctx context.Context, sessionID string) (SessionActivity, error) {
	var act SessionActivity
	var last sql.NullString
	if err := s.db.QueryRowContext(ctx,
		`SELECT MAX(at) FROM events WHERE session_id = ?`, sessionID).Scan(&last); err != nil {
		return act, fmt.Errorf("session activity: last event: %w", err)
	}
	if last.Valid {
		if t, err := time.Parse(time.RFC3339Nano, last.String); err == nil {
			act.LastEventAt = t
		}
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM events WHERE session_id = ? AND kind = ?
		   AND id > COALESCE((SELECT MAX(id) FROM events WHERE session_id = ? AND kind = ?), 0)`,
		sessionID, events.KindProviderPermissionDenied, sessionID, events.KindSessionTurnOutput,
	).Scan(&act.DeniedSinceOutput); err != nil {
		return act, fmt.Errorf("session activity: denials: %w", err)
	}
	return act, nil
}
