package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/hollis-labs/tether/internal/launchprofile"
)

// SessionRoute reads the immutable resolved route from the session record.
// A nil route means no routing, including sessions predating the feature.
func (s *Store) SessionRoute(ctx context.Context, id string) (*launchprofile.Route, error) {
	var raw sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT route_json FROM sessions WHERE id=?`, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrSessionNotFound, id)
	}
	if err != nil {
		return nil, err
	}
	if !raw.Valid {
		return nil, nil
	}
	var route launchprofile.Route
	if err := json.Unmarshal([]byte(raw.String), &route); err != nil {
		return nil, fmt.Errorf("invalid stored session route: %w", err)
	}
	return launchprofile.ValidateResolvedRoute(&route)
}
