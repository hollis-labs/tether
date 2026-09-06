package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// SessionProviderMapping records one provider-native session identifier for
// one canonical session, scoped by owner (the substrate that made the
// observation -- "tether" for everything Tether observes directly). This is
// the T02 replacement for logical_agents.claude_session_id, which is
// write-only in production (no code reads it back -- see T02 research) and
// shared across every provider kind that reports a session id rather than
// scoped per session. That column is left untouched for compatibility; new
// code records provider identity here instead.
type SessionProviderMapping struct {
	SessionID       string
	Owner           string
	Provider        string
	NativeSessionID sql.NullString
	CreatedAt       string
	UpdatedAt       string
}

// ErrProviderMappingNotFound is returned by GetSessionProviderMapping when no
// row matches the (sessionID, owner, provider) key.
var ErrProviderMappingNotFound = errors.New("store: provider mapping not found")

// UpsertSessionProviderMapping inserts or updates the (session_id, owner,
// provider) row. On insert, created_at and updated_at are both set to now;
// on update (the same key observed again, e.g. a later system/init event
// resolving a native id that was empty at session start), created_at is
// preserved and updated_at bumps. nativeSessionID may be empty -- a mapping
// row can name the provider/owner scope before any native id is known.
func (s *Store) UpsertSessionProviderMapping(sessionID, owner, provider, nativeSessionID string) error {
	if sessionID == "" || owner == "" || provider == "" {
		return fmt.Errorf("store: upsert session provider mapping: sessionID, owner and provider are required")
	}
	now := time.Now().UTC().Format(time.RFC3339)
	var native sql.NullString
	if nativeSessionID != "" {
		native = sql.NullString{String: nativeSessionID, Valid: true}
	}
	_, err := s.db.Exec(`
		INSERT INTO session_provider_mappings (session_id, owner, provider, native_session_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (session_id, owner, provider) DO UPDATE SET
			native_session_id = excluded.native_session_id,
			updated_at = excluded.updated_at`,
		sessionID, owner, provider, native, now, now)
	return err
}

// GetSessionProviderMapping returns the mapping for (sessionID, owner,
// provider), or ErrProviderMappingNotFound when absent.
func (s *Store) GetSessionProviderMapping(sessionID, owner, provider string) (SessionProviderMapping, error) {
	var m SessionProviderMapping
	err := s.db.QueryRow(`
		SELECT session_id, owner, provider, native_session_id, created_at, updated_at
		FROM session_provider_mappings WHERE session_id=? AND owner=? AND provider=?`,
		sessionID, owner, provider,
	).Scan(&m.SessionID, &m.Owner, &m.Provider, &m.NativeSessionID, &m.CreatedAt, &m.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return SessionProviderMapping{}, fmt.Errorf("%w: session=%s owner=%s provider=%s", ErrProviderMappingNotFound, sessionID, owner, provider)
	}
	if err != nil {
		return SessionProviderMapping{}, err
	}
	return m, nil
}

// ListSessionProviderMappings returns every provider mapping recorded for
// sessionID, ordered by provider for stable output. Empty (not nil) when
// none exist.
func (s *Store) ListSessionProviderMappings(sessionID string) ([]SessionProviderMapping, error) {
	rows, err := s.db.Query(`
		SELECT session_id, owner, provider, native_session_id, created_at, updated_at
		FROM session_provider_mappings WHERE session_id=? ORDER BY provider, owner`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SessionProviderMapping{}
	for rows.Next() {
		var m SessionProviderMapping
		if err := rows.Scan(&m.SessionID, &m.Owner, &m.Provider, &m.NativeSessionID, &m.CreatedAt, &m.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
