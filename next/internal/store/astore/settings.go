package astore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/hollis-labs/tether/next/internal/store"
)

// ErrSettingNotFound is returned when a requested setting does not exist.
var ErrSettingNotFound = errors.New("settings: not found")

// SettingScope is the precedence tier in the settings cascade: user, then project, then global (closest
// wins).
type SettingScope string

// Setting scopes.
const (
	ScopeGlobal  SettingScope = "global"
	ScopeProject SettingScope = "project"
	ScopeUser    SettingScope = "user"
)

// Setting is one scoped entry in the settings table. The cascade and the per-key rules (onboarding, the
// MCP discovery mode) belong to the settings domain, which the slice porting it adds on top.
type Setting struct {
	Scope     SettingScope
	ScopeID   string // empty for global; the project or user id otherwise
	Key       string
	ValueJSON string
	UpdatedAt time.Time
}

// Validate checks the scope and scope_id agree and the key is set.
func (s Setting) Validate() error {
	switch s.Scope {
	case ScopeGlobal:
		if s.ScopeID != "" {
			return errors.New("global scope must have empty scope_id")
		}
	case ScopeProject, ScopeUser:
		if s.ScopeID == "" {
			return fmt.Errorf("%s scope requires non-empty scope_id", s.Scope)
		}
	default:
		return fmt.Errorf("invalid settings scope %q", s.Scope)
	}
	if s.Key == "" {
		return errors.New("settings key cannot be empty")
	}
	return nil
}

// settingTime is SQLite's CURRENT_TIMESTAMP form, which the old store wrote: UTC, to the second.
const settingTime = "2006-01-02 15:04:05"

// SetSetting stores or updates a setting.
func (a *Store) SetSetting(ctx context.Context, s Setting) error {
	if err := s.Validate(); err != nil {
		return err
	}
	at := a.now().UTC().Format(settingTime)
	err := a.s.A().WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO settings (scope, scope_id, key, value_json, updated_at) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (scope, scope_id, key) DO UPDATE SET value_json = excluded.value_json, updated_at = excluded.updated_at`,
			string(s.Scope), s.ScopeID, s.Key, s.ValueJSON, at)
		return err
	})
	if err != nil {
		return fmt.Errorf("settings set (%s:%s:%s): %w", s.Scope, s.ScopeID, s.Key, err)
	}
	return nil
}

// scanSetting reads updated_at as text on both dialects (modernc returns the TIMESTAMP-declared column as
// time.Time, which database/sql renders as RFC3339 text).
func scanSetting(sc scanner) (Setting, error) {
	var out Setting
	var scope, updated string
	if err := sc.Scan(&scope, &out.ScopeID, &out.Key, &out.ValueJSON, &updated); err != nil {
		return Setting{}, err
	}
	out.Scope = SettingScope(scope)
	if t, err := time.Parse(time.RFC3339Nano, updated); err == nil {
		out.UpdatedAt = t.UTC()
	} else if t, err := time.Parse(settingTime, updated); err == nil {
		out.UpdatedAt = t
	}
	return out, nil
}

// GetSetting fetches one setting; ErrSettingNotFound when there is none.
func (a *Store) GetSetting(ctx context.Context, scope SettingScope, scopeID, key string) (*Setting, error) {
	out, err := scanSetting(a.s.A().Reader().QueryRowContext(ctx,
		`SELECT scope, scope_id, key, value_json, updated_at FROM settings WHERE scope = ? AND scope_id = ? AND key = ?`,
		string(scope), scopeID, key))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSettingNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("settings get (%s:%s:%s): %w", scope, scopeID, key, err)
	}
	return &out, nil
}

// DeleteSetting removes a setting; deleting one that does not exist is not an error.
func (a *Store) DeleteSetting(ctx context.Context, scope SettingScope, scopeID, key string) error {
	err := a.s.A().WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM settings WHERE scope = ? AND scope_id = ? AND key = ?`, string(scope), scopeID, key)
		return err
	})
	if err != nil {
		return fmt.Errorf("settings delete (%s:%s:%s): %w", scope, scopeID, key, err)
	}
	return nil
}

// ListSettings returns every setting of a scope and scope_id, ordered by key.
func (a *Store) ListSettings(ctx context.Context, scope SettingScope, scopeID string) ([]Setting, error) {
	rows, err := a.s.A().Reader().QueryContext(ctx,
		`SELECT scope, scope_id, key, value_json, updated_at FROM settings WHERE scope = ? AND scope_id = ? ORDER BY key`,
		string(scope), scopeID)
	if err != nil {
		return nil, fmt.Errorf("settings list (%s:%s): %w", scope, scopeID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []Setting
	for rows.Next() {
		s, err := scanSetting(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
