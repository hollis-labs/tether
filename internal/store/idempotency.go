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

// SessionIdempotency binds one idempotency key to the session its first
// request created (CW-20260930-0229). Only a digest of the request is kept.
type SessionIdempotency struct {
	Key           string
	Operation     string
	RequestDigest string
	SessionID     string
	// ResolvedParentSessionID is, for a resume, the parent the resolver
	// chose. Audit only: it is not part of the digest.
	ResolvedParentSessionID sql.NullString
}

func insertSessionIdempotency(tx *sql.Tx, sessionID string, key SessionIdempotency) error {
	if _, err := tx.Exec(`INSERT INTO session_idempotency
		(key, operation, request_digest, session_id, resolved_parent_session_id)
		VALUES (?, ?, ?, ?, ?)`,
		key.Key, key.Operation, key.RequestDigest, sessionID, key.ResolvedParentSessionID); err != nil {
		return fmt.Errorf("record idempotency key: %w", err)
	}
	return nil
}

// GetSessionIdempotency returns the record for key, or nil when the key has
// never been used.
func (s *Store) GetSessionIdempotency(key string) (*SessionIdempotency, error) {
	var rec SessionIdempotency
	err := s.db.QueryRow(`SELECT key, operation, request_digest, session_id, resolved_parent_session_id
		FROM session_idempotency WHERE key = ?`, key).
		Scan(&rec.Key, &rec.Operation, &rec.RequestDigest, &rec.SessionID, &rec.ResolvedParentSessionID)
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
