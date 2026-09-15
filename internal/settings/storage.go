package settings

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ErrNotFound is returned when a requested setting does not exist.
var ErrNotFound = errors.New("settings: not found")

// Storage manages persistent storage for settings in SQLite.
type Storage struct {
	db *sql.DB
}

// NewStorage creates a new Storage bound to the SQLite database.
func NewStorage(db *sql.DB) *Storage {
	return &Storage{db: db}
}

// Set stores or updates a setting.
func (s *Storage) Set(ctx context.Context, setting Setting) error {
	if err := setting.Validate(); err != nil {
		return err
	}

	query := `
		INSERT INTO settings (scope, scope_id, key, value_json, updated_at)
		VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT (scope, scope_id, key)
		DO UPDATE SET
			value_json = excluded.value_json,
			updated_at = CURRENT_TIMESTAMP;
	`
	_, err := s.db.ExecContext(ctx, query, setting.Scope, setting.ScopeID, setting.Key, setting.ValueJSON)
	if err != nil {
		return fmt.Errorf("settings set (%s:%s:%s): %w", setting.Scope, setting.ScopeID, setting.Key, err)
	}
	return nil
}

// Get fetches a single setting by scope, scope_id, and key.
func (s *Storage) Get(ctx context.Context, scope Scope, scopeID, key string) (*Setting, error) {
	query := `
		SELECT scope, scope_id, key, value_json, updated_at
		FROM settings
		WHERE scope = ? AND scope_id = ? AND key = ?;
	`
	var out Setting
	var updatedStr string
	err := s.db.QueryRowContext(ctx, query, scope, scopeID, key).Scan(
		&out.Scope,
		&out.ScopeID,
		&out.Key,
		&out.ValueJSON,
		&updatedStr,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("settings get (%s:%s:%s): %w", scope, scopeID, key, err)
	}

	if t, err := time.Parse(time.RFC3339, updatedStr); err == nil {
		out.UpdatedAt = t
	} else if t, err := time.Parse("2006-01-02 15:04:05", updatedStr); err == nil {
		out.UpdatedAt = t
	}

	return &out, nil
}

// Delete removes a setting row.
func (s *Storage) Delete(ctx context.Context, scope Scope, scopeID, key string) error {
	query := `DELETE FROM settings WHERE scope = ? AND scope_id = ? AND key = ?;`
	_, err := s.db.ExecContext(ctx, query, scope, scopeID, key)
	if err != nil {
		return fmt.Errorf("settings delete (%s:%s:%s): %w", scope, scopeID, key, err)
	}
	return nil
}

// ListByScope returns all settings configured for a given scope and scope_id.
func (s *Storage) ListByScope(ctx context.Context, scope Scope, scopeID string) ([]Setting, error) {
	query := `
		SELECT scope, scope_id, key, value_json, updated_at
		FROM settings
		WHERE scope = ? AND scope_id = ?
		ORDER BY key ASC;
	`
	rows, err := s.db.QueryContext(ctx, query, scope, scopeID)
	if err != nil {
		return nil, fmt.Errorf("settings list (%s:%s): %w", scope, scopeID, err)
	}
	defer rows.Close() //nolint:errcheck

	var out []Setting
	for rows.Next() {
		var item Setting
		var updatedStr string
		if err := rows.Scan(&item.Scope, &item.ScopeID, &item.Key, &item.ValueJSON, &updatedStr); err != nil {
			return nil, err
		}
		if t, err := time.Parse(time.RFC3339, updatedStr); err == nil {
			item.UpdatedAt = t
		} else if t, err := time.Parse("2006-01-02 15:04:05", updatedStr); err == nil {
			item.UpdatedAt = t
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// SetOnboarding stores typed OnboardingSettings at the given scope.
func (s *Storage) SetOnboarding(ctx context.Context, scope Scope, scopeID string, ob OnboardingSettings) error {
	b, err := json.Marshal(ob)
	if err != nil {
		return fmt.Errorf("marshal onboarding settings: %w", err)
	}
	return s.Set(ctx, Setting{
		Scope:     scope,
		ScopeID:   scopeID,
		Key:       KeyOnboarding,
		ValueJSON: string(b),
	})
}

// GetOnboarding retrieves typed OnboardingSettings at the given scope.
// Returns nil, nil if no setting is configured at that tier.
func (s *Storage) GetOnboarding(ctx context.Context, scope Scope, scopeID string) (*OnboardingSettings, error) {
	setting, err := s.Get(ctx, scope, scopeID, KeyOnboarding)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out OnboardingSettings
	if err := json.Unmarshal([]byte(setting.ValueJSON), &out); err != nil {
		return nil, fmt.Errorf("unmarshal onboarding settings: %w", err)
	}
	return &out, nil
}

// ResolveEffectiveOnboarding calculates the effective OnboardingSettings across
// Global, Project, and User tiers using the closest-wins cascade.
func (s *Storage) ResolveEffectiveOnboarding(ctx context.Context, projectID, userID string) (OnboardingSettings, error) {
	global, err := s.GetOnboarding(ctx, ScopeGlobal, "")
	if err != nil {
		return OnboardingSettings{}, fmt.Errorf("get global onboarding settings: %w", err)
	}

	var project *OnboardingSettings
	if projectID != "" {
		p, err := s.GetOnboarding(ctx, ScopeProject, projectID)
		if err != nil {
			return OnboardingSettings{}, fmt.Errorf("get project onboarding settings: %w", err)
		}
		project = p
	}

	var user *OnboardingSettings
	if userID != "" {
		u, err := s.GetOnboarding(ctx, ScopeUser, userID)
		if err != nil {
			return OnboardingSettings{}, fmt.Errorf("get user onboarding settings: %w", err)
		}
		user = u
	}

	return ResolveEffectiveOnboarding(global, project, user), nil
}
