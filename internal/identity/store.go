package identity

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

type Store struct {
	db       *sql.DB
	streamMu sync.Mutex
	streams  map[string]map[*deviceStream]struct{}
}

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
	if p.ID == "" || (p.Kind != "operator" && p.Kind != "session" && p.Kind != "service" && p.Kind != "interactive" && p.Kind != "device") {
		return "", fmt.Errorf("principal id and valid kind required")
	}
	if p.Kind == "device" {
		if p.ID == OperatorID || p.ExpiresAt == nil {
			return "", fmt.Errorf("expiring non-operator device required")
		}
		var err error
		p.Scopes, err = NormalizeDeviceScopes(p.Scopes)
		if err != nil {
			return "", err
		}
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
		if p.Kind == "device" {
			expires = p.ExpiresAt.UTC().Format(credentialTimeFormat)
		}
	}
	result, err := writer.ExecContext(ctx, `INSERT INTO principals
        (principal_id, kind, display, token_hash, scopes_json, session_id, addresses_json,
         created_by, created_at, expires_at, launch_attribution) SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
         WHERE ? != 'session' OR EXISTS (SELECT 1 FROM sessions WHERE id=? AND state NOT IN ('completed','failed','killed','orphaned'))`,
		p.ID, p.Kind, p.Display, HashToken(token), string(scopes), p.SessionID,
		string(addresses), p.CreatedBy, p.CreatedAt.Format(time.RFC3339Nano), expires, p.LaunchAttribution, p.Kind, p.SessionID)
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
	return s.verifyHash(ctx, hash)
}

func (s *Store) verifyHash(ctx context.Context, hash string) (Principal, error) {
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
	// The credential hash matched an active row: the only place, besides the
	// middleware for an injected Verifier, that a Principal becomes Verified.
	p.Verified = true
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
	if p.Kind == "device" {
		if p.ID == OperatorID || p.ExpiresAt == nil {
			return Principal{}, ErrInvalidToken
		}
		p.Scopes, err = NormalizeDeviceScopes(p.Scopes)
		if err != nil {
			return Principal{}, fmt.Errorf("invalid persisted device scopes")
		}
	}
	return p, nil
}

func (s *Store) Revoke(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE principals SET revoked_at = COALESCE(revoked_at, ?) WHERE principal_id = ?`, time.Now().UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		return fmt.Errorf("revoke principal: %w", err)
	}
	s.cancelDeviceStreams(id)
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
	// A token-specific revoke must not cancel a different credential sharing
	// a legacy principal id; watchers recheck their actual hash immediately.
	s.checkDeviceStreams()
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
        (at, principal_id, session_id, mode, authentication, method, route, route_pattern, required_scope, would_allow, event, verified)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, o.At.UTC().Format(time.RFC3339Nano),
		o.PrincipalID, o.SessionID, o.Mode, o.Authentication, o.Method, o.Route,
		o.RoutePattern, o.RequiredScope, wouldAllowColumn(o.WouldAllow), o.Event, o.Authentication == "verified")
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

func wouldAllowColumn(v *bool) any {
	if v == nil {
		return nil
	}
	if *v {
		return 1
	}
	return 0
}

// SessionOwner returns the principal that launched a session, read from the
// session's own live credential: the created_by of its unrevoked, unexpired
// principal row. It is empty when the session has no such credential (identity
// observe or off mode, a revoked or expired credential), which callers must
// treat as unverified rather than as ownerless-and-trusted.
func (s *Store) SessionOwner(ctx context.Context, sessionID string) (string, error) {
	if s == nil || s.db == nil || sessionID == "" {
		return "", fmt.Errorf("identity store unavailable")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT created_by, expires_at FROM principals
        WHERE kind = 'session' AND session_id = ? AND revoked_at IS NULL
        ORDER BY created_at DESC`, sessionID)
	if err != nil {
		return "", fmt.Errorf("lookup session owner: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var owner string
		var expires sql.NullString
		if err = rows.Scan(&owner, &expires); err != nil {
			return "", fmt.Errorf("lookup session owner: %w", err)
		}
		if expires.Valid {
			at, parseErr := time.Parse(time.RFC3339Nano, expires.String)
			if parseErr != nil || !at.After(time.Now()) {
				continue
			}
		}
		return owner, nil
	}
	if err = rows.Err(); err != nil {
		return "", fmt.Errorf("lookup session owner: %w", err)
	}
	return "", nil
}

// SessionGrantedScopes returns the scopes most recently granted to a session's
// credential, whether or not that credential is still live: it answers "what
// authority did this session hold", which bounds anything it spawns or that is
// resumed from it. The second result is false when the session never had a
// credential.
func (s *Store) SessionGrantedScopes(ctx context.Context, sessionID string) ([]string, bool, error) {
	if s == nil || s.db == nil || sessionID == "" {
		return nil, false, fmt.Errorf("identity store unavailable")
	}
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT scopes_json FROM principals WHERE kind = 'session' AND session_id = ? ORDER BY created_at DESC LIMIT 1`, sessionID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("lookup session scopes: %w", err)
	}
	var scopes []string
	if err := json.Unmarshal([]byte(raw), &scopes); err != nil {
		return nil, false, fmt.Errorf("decode session scopes: %w", err)
	}
	return scopes, true, nil
}

// Launch attributions that mean a session credential was born with the full
// profile ceiling and nothing bounding it.
const (
	AttributionInternalUnattributed = "internal_unattributed"
	AttributionAnonymousExternal    = "anonymous_external"
	// AttributionLegacy labels a credential from before attribution was
	// recorded whose launcher is unknown (created_by empty).
	AttributionLegacy = "legacy_unknown_launcher"
)

// UnboundedSession is a live session credential that nothing bounded at launch.
type UnboundedSession struct {
	SessionID   string
	Attribution string
	CreatedAt   time.Time
	ExpiresAt   *time.Time
}

// UnboundedSessions lists live (unrevoked, unexpired) session credentials that
// were born without a bounding authority: recorded as anonymous_external or
// internal_unattributed, or legacy ones (no attribution recorded and an empty
// created_by, i.e. launched before the launcher was recorded). Switching
// identity to enforce must not leave these standing.
func (s *Store) UnboundedSessions(ctx context.Context) ([]UnboundedSession, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("identity store unavailable")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT session_id, launch_attribution, created_by, created_at, expires_at
        FROM principals WHERE kind = 'session' AND revoked_at IS NULL
          AND (launch_attribution IN (?, ?) OR (launch_attribution = '' AND created_by = ''))
        ORDER BY created_at`, AttributionAnonymousExternal, AttributionInternalUnattributed)
	if err != nil {
		return nil, fmt.Errorf("list unbounded session credentials: %w", err)
	}
	defer func() { _ = rows.Close() }()
	now := time.Now()
	var out []UnboundedSession
	for rows.Next() {
		var u UnboundedSession
		var createdBy, created string
		var expires sql.NullString
		if err = rows.Scan(&u.SessionID, &u.Attribution, &createdBy, &created, &expires); err != nil {
			return nil, fmt.Errorf("list unbounded session credentials: %w", err)
		}
		if u.Attribution == "" {
			u.Attribution = AttributionLegacy
		}
		if u.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
			return nil, fmt.Errorf("decode creation time: %w", err)
		}
		if expires.Valid {
			at, perr := time.Parse(time.RFC3339Nano, expires.String)
			if perr != nil {
				return nil, fmt.Errorf("decode expiry: %w", perr)
			}
			if !at.After(now) {
				continue // already dead
			}
			u.ExpiresAt = &at
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// RevokeUnbounded revokes the credentials UnboundedSessions lists and returns
// how many it revoked. It is the explicit step behind switching to enforce, and
// it is all or nothing: the list is revoked in one transaction, so an error
// leaves every credential as it was and the count is never partial.
func (s *Store) RevokeUnbounded(ctx context.Context) (int, error) {
	list, err := s.UnboundedSessions(ctx)
	if err != nil {
		return 0, err
	}
	if len(list) == 0 {
		return 0, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("revoke unbounded session credentials: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	at := time.Now().UTC().Format(time.RFC3339Nano)
	for _, u := range list {
		if _, err = tx.ExecContext(ctx, `UPDATE principals SET revoked_at = COALESCE(revoked_at, ?) WHERE kind = 'session' AND session_id = ?`, at, u.SessionID); err != nil {
			return 0, fmt.Errorf("revoke unbounded session credential %s (nothing was revoked): %w", u.SessionID, err)
		}
	}
	if err = tx.Commit(); err != nil {
		return 0, fmt.Errorf("revoke unbounded session credentials (nothing was revoked): %w", err)
	}
	return len(list), nil
}
