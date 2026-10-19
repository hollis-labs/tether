package identity

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// CredentialMetadata separates a credential record from its principal: rotation
// can leave more than one credential for the same subject. Neither the bearer
// nor its stored hash belongs in an inventory response.
type CredentialMetadata struct {
	CredentialID int64      `json:"credential_id"`
	PrincipalID  string     `json:"principal_id"`
	Kind         string     `json:"kind"`
	Display      string     `json:"display"`
	Scopes       []string   `json:"scopes"`
	CreatedBy    string     `json:"created_by,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	ExpiresAt    *time.Time `json:"expires_at,omitempty"`
	RevokedAt    *time.Time `json:"revoked_at,omitempty"`
	LastUsedAt   *time.Time `json:"last_used_at,omitempty"`
}

// CredentialQuery selects a page of credentials; a zero Limit defaults to 100.
type CredentialQuery struct {
	Kind        string
	PrincipalID string
	After       int64
	Limit       int
}

// Validate rejects unsupported filters and unbounded page requests.
func (q CredentialQuery) Validate() error {
	if q.After < 0 || q.Limit < 0 || q.Limit > 200 || len(q.PrincipalID) > 512 {
		return fmt.Errorf("after must be nonnegative, limit between 1 and 200, and principal at most 512 bytes")
	}
	switch q.Kind {
	case "", "operator", "session", "service", "interactive", "device":
		return nil
	default:
		return fmt.Errorf("unknown principal kind")
	}
}

// CredentialPage carries a cursor only when another credential follows.
type CredentialPage struct {
	Credentials []CredentialMetadata `json:"credentials"`
	NextAfter   int64                `json:"next_after,omitempty"`
}

// ListCredentials is a bounded, metadata-only read. Expired and revoked rows
// remain visible so an operator can distinguish absence from inactive custody.
func (s *Store) ListCredentials(ctx context.Context, q CredentialQuery) (CredentialPage, error) {
	page := CredentialPage{Credentials: []CredentialMetadata{}}
	if err := q.Validate(); err != nil {
		return page, err
	}
	if q.Limit == 0 {
		q.Limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, principal_id, kind, display,
		scopes_json, created_by, created_at, expires_at, revoked_at, last_used_at
		FROM principals WHERE id > ? AND (? = '' OR kind = ?)
		AND (? = '' OR principal_id = ?) ORDER BY id LIMIT ?`,
		q.After, q.Kind, q.Kind, q.PrincipalID, q.PrincipalID, q.Limit+1)
	if err != nil {
		return page, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		if len(page.Credentials) == q.Limit {
			page.NextAfter = page.Credentials[len(page.Credentials)-1].CredentialID
			break
		}
		var c CredentialMetadata
		var scopes, created string
		var expires, revoked, used sql.NullString
		if err := rows.Scan(&c.CredentialID, &c.PrincipalID, &c.Kind, &c.Display,
			&scopes, &c.CreatedBy, &created, &expires, &revoked, &used); err != nil {
			return CredentialPage{}, err
		}
		if err := json.Unmarshal([]byte(scopes), &c.Scopes); err != nil {
			return CredentialPage{}, fmt.Errorf("invalid credential scopes")
		}
		if c.Scopes == nil {
			c.Scopes = []string{}
		}
		if c.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
			return CredentialPage{}, fmt.Errorf("invalid credential creation time")
		}
		for _, field := range []struct {
			value sql.NullString
			out   **time.Time
		}{{expires, &c.ExpiresAt}, {revoked, &c.RevokedAt}, {used, &c.LastUsedAt}} {
			if field.value.Valid {
				at, err := time.Parse(time.RFC3339Nano, field.value.String)
				if err != nil {
					return CredentialPage{}, fmt.Errorf("invalid credential lifecycle time")
				}
				*field.out = &at
			}
		}
		page.Credentials = append(page.Credentials, c)
	}
	return page, rows.Err()
}
