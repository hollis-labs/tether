package identity

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// VerifyTx is read-only. It preserves Verify's hashed lookup, revocation,
// expiration and principal decoding within the caller's actual transaction.
func VerifyTx(ctx context.Context, tx *sql.Tx, token string) (Principal, error) {
	if tx == nil {
		return Principal{}, fmt.Errorf("identity store unavailable")
	}
	if !validToken(token) {
		return Principal{}, ErrInvalidToken
	}
	hash := HashToken(token)
	var p Principal
	var scopes, addresses, created string
	var revoked, expires sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT principal_id, kind, display,
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
