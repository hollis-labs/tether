package identity

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"time"
	"unicode"
)

var ErrDeviceNotFound = errors.New("device not found")

type Device struct {
	ID            string     `json:"id"`
	Label         string     `json:"label"`
	Scopes        []string   `json:"scopes"`
	CreatedAt     time.Time  `json:"created_at"`
	ExpiresAt     time.Time  `json:"expires_at"`
	RevokedAt     *time.Time `json:"revoked_at,omitempty"`
	LastUsedAt    *time.Time `json:"last_used_at,omitempty"`
	RemoteAddress string     `json:"remote_address,omitempty"`
	UserAgent     string     `json:"user_agent,omitempty"`
}

func (s *Store) ListDevices(ctx context.Context) ([]Device, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT principal_id, display, scopes_json, created_at, expires_at, revoked_at, last_used_at, last_remote_addr, last_user_agent FROM principals WHERE kind = 'device' ORDER BY created_at, principal_id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	devices := make([]Device, 0)
	for rows.Next() {
		var d Device
		var scopes, created, expires string
		var revoked, used sql.NullString
		if err := rows.Scan(&d.ID, &d.Label, &scopes, &created, &expires, &revoked, &used, &d.RemoteAddress, &d.UserAgent); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(scopes), &d.Scopes); err != nil {
			return nil, err
		}
		if d.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
			return nil, err
		}
		if d.ExpiresAt, err = time.Parse(time.RFC3339Nano, expires); err != nil {
			return nil, err
		}
		if revoked.Valid {
			at, err := time.Parse(time.RFC3339Nano, revoked.String)
			if err != nil {
				return nil, err
			}
			d.RevokedAt = &at
		}
		if used.Valid {
			at, err := time.Parse(time.RFC3339Nano, used.String)
			if err != nil {
				return nil, err
			}
			d.LastUsedAt = &at
		}
		devices = append(devices, d)
	}
	return devices, rows.Err()
}

func (s *Store) RevokeDevice(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE principals SET revoked_at = COALESCE(revoked_at, ?) WHERE kind = 'device' AND principal_id = ?`, time.Now().UTC().Format(credentialTimeFormat), id)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrDeviceNotFound
	}
	s.cancelDeviceStreams(id)
	return nil
}

// RenewDevice retains the existing hash-only credential, extends its lifetime
// while valid, and can only narrow the database's current scopes. No operator,
// session, expired or revoked credential can use this path.
func (s *Store) RenewDevice(ctx context.Context, p Principal, hash string, requested []string) (time.Time, []string, error) {
	if p.Kind != "device" || p.ID == OperatorID || len(hash) != 64 {
		return time.Time{}, nil, ErrInvalidToken
	}
	scopes, err := NarrowDeviceScopes(p.Scopes, requested)
	if err != nil {
		return time.Time{}, nil, err
	}
	encoded, _ := json.Marshal(scopes)
	now := time.Now().UTC()
	expires := now.Add(DeviceLifetime)
	result, err := s.db.ExecContext(ctx, `UPDATE principals SET expires_at = ?, scopes_json = ? WHERE kind = 'device' AND principal_id = ? AND token_hash = ? AND revoked_at IS NULL AND expires_at > ? AND NOT EXISTS (SELECT 1 FROM json_each(?) requested WHERE requested.value NOT IN (SELECT value FROM json_each(principals.scopes_json)))`, expires.Format(credentialTimeFormat), string(encoded), p.ID, hash, now.Format(credentialTimeFormat), string(encoded))
	if err != nil {
		return time.Time{}, nil, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return time.Time{}, nil, err
	}
	if n != 1 {
		return time.Time{}, nil, ErrInvalidToken
	}
	if !slices.Equal(scopes, p.Scopes) {
		s.cancelDeviceStreams(p.ID)
	}
	return expires, scopes, nil
}

// RecordDeviceUse commits device use and its metadata together with the existing identity audit.
// Callers never pass bearer headers, token strings, URL queries or bodies here.
func (s *Store) RecordDeviceUse(ctx context.Context, p Principal, o Observation, remoteAddress, userAgent string) error {
	if p.Kind != "device" || p.ID == "" || p.ID == OperatorID {
		return ErrInvalidToken
	}
	address := ""
	if host, _, err := net.SplitHostPort(remoteAddress); err == nil {
		if ip := net.ParseIP(host); ip != nil {
			address = ip.String()
		}
	}
	userAgent = safeDeviceMetadata(userAgent, 256)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `UPDATE principals SET last_used_at = ?, last_remote_addr = ?, last_user_agent = ? WHERE kind = 'device' AND principal_id = ? AND revoked_at IS NULL`, o.At.UTC().Format(credentialTimeFormat), address, userAgent, p.ID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil || n != 1 {
		return fmt.Errorf("device use unavailable")
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO identity_audit (at, principal_id, session_id, mode, authentication, method, route) VALUES (?, ?, '', ?, ?, ?, ?)`, o.At.UTC().Format(credentialTimeFormat), p.ID, string(Enforce), o.Authentication, o.Method, o.Route)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func safeDeviceMetadata(value string, limit int) string {
	if strings.Contains(value, "tth_") || strings.Contains(value, "tpg_") {
		return "<redacted>"
	}
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, value)
	if len(value) > limit {
		value = value[:limit]
	}
	return strings.ToValidUTF8(value, "")
}
