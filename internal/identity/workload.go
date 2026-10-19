package identity

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"
)

// CheckWorkloadCredential reads the exact credential used for a direct workload
// mint. A different credential for the same subject cannot keep it alive.
// This is lifecycle eligibility, not authentication of a bearer token.
func (s *Store) CheckWorkloadCredential(ctx context.Context, id int64, subject, kind, scope string, now time.Time) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("identity store unavailable")
	}
	if id <= 0 || (kind != "service" && kind != "device") {
		return ErrInvalidToken
	}
	var storedSubject, storedKind, scopes string
	var revoked, expires sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT principal_id, kind, scopes_json, revoked_at, expires_at FROM principals WHERE id = ?`, id).Scan(&storedSubject, &storedKind, &scopes, &revoked, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrInvalidToken
	}
	if err != nil {
		return fmt.Errorf("current workload lookup: %w", err)
	}
	if storedSubject != subject || storedKind != kind || revoked.Valid {
		return ErrInvalidToken
	}
	if kind == "device" && !expires.Valid {
		return ErrInvalidToken
	}
	if expires.Valid {
		expiry, err := time.Parse(time.RFC3339Nano, expires.String)
		if err != nil {
			return fmt.Errorf("invalid workload expiry: %w", err)
		}
		if !now.Before(expiry) {
			return ErrInvalidToken
		}
	}
	var granted []string
	if err := json.Unmarshal([]byte(scopes), &granted); err != nil {
		return fmt.Errorf("invalid workload scopes: %w", err)
	}
	if !slices.Equal(granted, []string{scope}) {
		return ErrInvalidToken
	}
	return nil
}
