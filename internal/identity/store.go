package identity

import (
	"context"
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
	return mint(ctx, s.db, p)
}

type principalWriter interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

// MintSessionForLaunch replaces stale credentials only while the session is
// still created. Revocation and replacement commit atomically, so an interrupted
// launch can retry without revoking a running session's credential.
func (s *Store) MintSessionForLaunch(ctx context.Context, p Principal) (string, error) {
	if p.Kind != "session" || p.SessionID == "" {
		return "", fmt.Errorf("session principal required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin session credential: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var state string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM sessions WHERE id = ?`, p.SessionID).Scan(&state); err != nil {
		return "", fmt.Errorf("load credential session: %w", err)
	}
	if state != "created" {
		return "", fmt.Errorf("session credential replacement requires created state")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE principals SET revoked_at = COALESCE(revoked_at, ?) WHERE kind = 'session' AND session_id = ? AND revoked_at IS NULL`, time.Now().UTC().Format(time.RFC3339Nano), p.SessionID); err != nil {
		return "", fmt.Errorf("revoke stale session credential: %w", err)
	}
	token, err := mint(ctx, tx, p)
	if err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit session credential: %w", err)
	}
	return token, nil
}

func mint(ctx context.Context, writer principalWriter, p Principal) (string, error) {
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
	result, err := writer.ExecContext(ctx, `INSERT INTO principals
        (principal_id, kind, display, token_hash, scopes_json, session_id, addresses_json,
         created_by, created_at, expires_at) SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
         WHERE ? != 'session' OR EXISTS (SELECT 1 FROM sessions WHERE id=? AND state NOT IN ('completed','failed','killed','orphaned'))`,
		p.ID, p.Kind, p.Display, HashToken(token), string(scopes), p.SessionID,
		string(addresses), p.CreatedBy, p.CreatedAt.Format(time.RFC3339Nano), expires, p.Kind, p.SessionID)
	if err != nil {
		return "", fmt.Errorf("persist principal: %w", err)
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return "", fmt.Errorf("cannot mint a principal for an inactive session")
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
	var scopes, addresses, created string
	var revoked, expires sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT principal_id, kind, display,
        scopes_json, session_id, addresses_json, created_by, created_at,
        revoked_at, expires_at FROM principals WHERE token_hash = ?`, hash).Scan(
		&p.ID, &p.Kind, &p.Display, &scopes, &p.SessionID,
		&addresses, &p.CreatedBy, &created, &revoked, &expires)
	// Verification uses an indexed lookup of a cryptographic token hash. The
	// database lookup is not constant-time; no token material is compared here.
	if errors.Is(err, sql.ErrNoRows) {
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
	_, err := s.db.ExecContext(ctx, `UPDATE principals SET revoked_at = COALESCE(revoked_at, ?) WHERE principal_id = ?`, time.Now().UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		return fmt.Errorf("revoke principal: %w", err)
	}
	return nil
}

// RevokeToken revokes only the credential minted by one launch attempt.
func (s *Store) RevokeToken(ctx context.Context, token string) error {
	if !validToken(token) {
		return ErrInvalidToken
	}
	_, err := s.db.ExecContext(ctx, `UPDATE principals SET revoked_at = COALESCE(revoked_at, ?) WHERE token_hash = ?`, time.Now().UTC().Format(time.RFC3339Nano), HashToken(token))
	if err != nil {
		return fmt.Errorf("revoke credential: %w", err)
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

// HasPrincipal prevents an absent file from silently rotating a prior operator.
func (s *Store) HasPrincipal(ctx context.Context, id string) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM principals WHERE principal_id = ?)`, id).Scan(&exists)
	return exists, err
}
