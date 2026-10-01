package identity

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type Store struct{ db *sql.DB }

func NewStore(db *sql.DB) *Store { return &Store{db: db} }

// Mint persists only a token hash. The raw token is returned once for delivery.
func (s *Store) Mint(ctx context.Context, p Principal) (string, error) {
	if p.ID == "" || (p.Kind != "operator" && p.Kind != "session" && p.Kind != "service" && p.Kind != "interactive") {
		return "", fmt.Errorf("principal id and valid kind required")
	}
	if (p.Kind == "session") != (p.SessionID != "") {
		return "", fmt.Errorf("session id is required only for session principals")
	}
	if p.RevokedAt != nil || (p.ExpiresAt != nil && !p.ExpiresAt.After(time.Now())) {
		return "", fmt.Errorf("cannot mint an inactive principal")
	}
	token, err := NewToken()
	if err != nil {
		return "", err
	}
	p.CreatedAt = time.Now().UTC()
	scopes, err := json.Marshal(p.Scopes)
	if err != nil {
		return "", fmt.Errorf("encode scopes: %w", err)
	}
	addresses, err := json.Marshal(p.Addresses)
	if err != nil {
		return "", fmt.Errorf("encode addresses: %w", err)
	}
	var expires any
	if p.ExpiresAt != nil {
		expires = p.ExpiresAt.UTC().Format(time.RFC3339Nano)
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO principals
        (id, kind, display, token_hash, scopes_json, session_id, addresses_json,
         created_by, created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.Kind, p.Display, HashToken(token), string(scopes), p.SessionID,
		string(addresses), p.CreatedBy, p.CreatedAt.Format(time.RFC3339Nano), expires)
	if err != nil {
		return "", fmt.Errorf("persist principal: %w", err)
	}
	return token, nil
}

func (s *Store) Verify(ctx context.Context, token string) (Principal, error) {
	if s == nil || s.db == nil {
		return Principal{}, fmt.Errorf("identity store unavailable")
	}
	if !validToken(token) {
		return Principal{}, ErrInvalidToken
	}
	hash := HashToken(token)
	var p Principal
	var storedHash, scopes, addresses, created string
	var revoked, expires sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT id, kind, display, token_hash,
        scopes_json, session_id, addresses_json, created_by, created_at,
        revoked_at, expires_at FROM principals WHERE token_hash = ?`, hash).Scan(
		&p.ID, &p.Kind, &p.Display, &storedHash, &scopes, &p.SessionID,
		&addresses, &p.CreatedBy, &created, &revoked, &expires)
	// Equal-length comparison on both hit and miss; lookup uses the hash index.
	if errors.Is(err, sql.ErrNoRows) {
		storedHash = HashToken("")
	}
	match := subtle.ConstantTimeCompare([]byte(storedHash), []byte(hash))
	if errors.Is(err, sql.ErrNoRows) || (err == nil && match != 1) {
		return Principal{}, ErrInvalidToken
	}
	if err != nil {
		return Principal{}, fmt.Errorf("lookup principal: %w", err)
	}
	if revoked.Valid {
		return Principal{}, ErrInvalidToken
	}
	if err := json.Unmarshal([]byte(scopes), &p.Scopes); err != nil {
		return Principal{}, fmt.Errorf("decode scopes: %w", err)
	}
	if err := json.Unmarshal([]byte(addresses), &p.Addresses); err != nil {
		return Principal{}, fmt.Errorf("decode addresses: %w", err)
	}
	p.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
	if err != nil {
		return Principal{}, fmt.Errorf("decode creation time: %w", err)
	}
	if expires.Valid {
		at, parseErr := time.Parse(time.RFC3339Nano, expires.String)
		if parseErr != nil {
			return Principal{}, fmt.Errorf("decode expiry: %w", parseErr)
		}
		if !at.After(time.Now()) {
			return Principal{}, ErrInvalidToken
		}
		p.ExpiresAt = &at
	}
	return p, nil
}

func (s *Store) Revoke(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE principals SET revoked_at = COALESCE(revoked_at, ?) WHERE id = ?`, time.Now().UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		return fmt.Errorf("revoke principal: %w", err)
	}
	return nil
}

func (s *Store) RevokeSession(ctx context.Context, sessionID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE principals SET revoked_at = COALESCE(revoked_at, ?) WHERE kind = 'session' AND session_id = ?`, time.Now().UTC().Format(time.RFC3339Nano), sessionID)
	if err != nil {
		return fmt.Errorf("revoke session principal: %w", err)
	}
	return nil
}

func (s *Store) RecordObservation(ctx context.Context, o Observation) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO identity_audit
        (at, principal_id, session_id, mode, authentication, method, route)
        VALUES (?, ?, ?, ?, ?, ?, ?)`, o.At.UTC().Format(time.RFC3339Nano),
		o.PrincipalID, o.SessionID, o.Mode, o.Authentication, o.Method, o.Route)
	if err != nil {
		return fmt.Errorf("persist identity observation: %w", err)
	}
	return nil
}
