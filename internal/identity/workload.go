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

const (
	BasaliskStatusAudience   = "basalisk-status"
	TachyonAssuranceAudience = "tachyon-assurance"
)

// WorkloadScopesAllowed recognizes closed workload profiles, without wildcard
// or broad Tether capabilities. A Tachyon caller can hold any nonempty subset
// of the three route scopes, including an outcome-only recovery credential.
func WorkloadScopesAllowed(audience string, scopes []string) bool {
	if audience == BasaliskStatusAudience {
		return slices.Equal(scopes, []string{BasaliskStatusScope})
	}
	if audience != TachyonAssuranceAudience || len(scopes) == 0 || len(scopes) > 3 {
		return false
	}
	seen := make(map[string]bool, len(scopes))
	for _, scope := range scopes {
		if seen[scope] || (scope != TachyonItemsScope && scope != TachyonCompletionsScope && scope != TachyonOutcomesScope) {
			return false
		}
		seen[scope] = true
	}
	return true
}

// WorkloadScopesCover requires both grants and requested scopes to belong to
// the same closed profile, then checks exact membership without expansion.
func WorkloadScopesCover(audience string, granted, requested []string) bool {
	if !WorkloadScopesAllowed(audience, granted) || !WorkloadScopesAllowed(audience, requested) {
		return false
	}
	for _, scope := range requested {
		if !slices.Contains(granted, scope) {
			return false
		}
	}
	return true
}

// CheckWorkloadCredential reads the exact credential used for a direct workload
// mint. A different credential for the same subject cannot keep it alive.
// This is lifecycle eligibility, not authentication of a bearer token.
func (s *Store) CheckWorkloadCredential(ctx context.Context, id int64, subject, kind, audience string, requested []string, now time.Time) error {
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
	if !WorkloadScopesCover(audience, granted, requested) {
		return ErrInvalidToken
	}
	return nil
}
