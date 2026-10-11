package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// ErrIdempotencyConflict reports an idempotency key reused with a request
// whose digest differs from the one that first claimed it. The key stays
// bound to its original session.
var ErrIdempotencyConflict = errors.New("idempotency key was already used with a different request")

// Idempotent operations. A key claimed by one operation conflicts with a
// request for the other.
const (
	IdempotencyOpCreate = "create"
	IdempotencyOpResume = "resume"
)

// Idempotency principals (CW-20261010-0263). A key is unique per principal, not
// globally. Real callers are claimed under their authenticated principal id.
// The reserved ids below are not principal ids, which are URNs or device ids.
const (
	// IdempotencyPrincipalAnonymous scopes callers the daemon could not
	// authenticate (observe mode). They share one space, as everyone once did.
	IdempotencyPrincipalAnonymous = "anonymous"
	// IdempotencyPrincipalTeam scopes keys the daemon's team runtime derives.
	IdempotencyPrincipalTeam = "team-runtime"
	// IdempotencyPrincipalLegacy marks rows from before keys were scoped
	// (migration 0066). They are replayable but never claimed.
	IdempotencyPrincipalLegacy = "legacy"
)

// SessionIdempotency binds one idempotency key to the session its first
// request created (CW-20260930-0229). Only a digest of the request is kept.
type SessionIdempotency struct {
	// PrincipalID scopes Key. Required when recording a key.
	PrincipalID   string
	Key           string
	Operation     string
	RequestDigest string
	SessionID     string
	// ResolvedParentSessionID is, for a resume, the parent the resolver
	// chose. Audit only: it is not part of the digest.
	ResolvedParentSessionID sql.NullString
}

func insertSessionIdempotency(tx *sql.Tx, sessionID string, key SessionIdempotency) error {
	if key.PrincipalID == "" || key.PrincipalID == IdempotencyPrincipalLegacy {
		return fmt.Errorf("record idempotency key: principal %q cannot claim a key", key.PrincipalID)
	}
	if _, err := tx.Exec(`INSERT INTO session_idempotency
		(principal_id, key, operation, request_digest, session_id, resolved_parent_session_id)
		VALUES (?, ?, ?, ?, ?, ?)`,
		key.PrincipalID, key.Key, key.Operation, key.RequestDigest, sessionID, key.ResolvedParentSessionID); err != nil {
		return fmt.Errorf("record idempotency key: %w", err)
	}
	return nil
}

// GetSessionIdempotency returns principal's record for key, or nil when that
// principal has never used it. A pre-scoping ("legacy") row for the key is
// returned when the principal has none of its own, so a retry that began
// before the upgrade still replays; it is never returned to claim a key.
func (s *Store) GetSessionIdempotency(principal, key string) (*SessionIdempotency, error) {
	var rec SessionIdempotency
	err := s.db.QueryRow(`SELECT principal_id, key, operation, request_digest, session_id, resolved_parent_session_id
		FROM session_idempotency WHERE key = ? AND principal_id IN (?, ?)
		ORDER BY principal_id = ? DESC LIMIT 1`, key, principal, IdempotencyPrincipalLegacy, principal).
		Scan(&rec.PrincipalID, &rec.Key, &rec.Operation, &rec.RequestDigest, &rec.SessionID, &rec.ResolvedParentSessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &rec, nil
}

// SessionHasIdempotencyKey reports whether sessionID was created by a keyed
// request, which makes its launch idempotent.
func (s *Store) SessionHasIdempotencyKey(sessionID string) (bool, error) {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(1) FROM session_idempotency WHERE session_id = ?`, sessionID).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}
