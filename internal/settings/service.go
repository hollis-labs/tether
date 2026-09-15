package settings

import (
	"context"
	"fmt"
)

// Service provides high-level operations for settings management and
// cascade resolution.
type Service struct {
	storage *Storage
}

// NewService constructs a settings Service backed by storage.
func NewService(storage *Storage) *Service {
	return &Service{storage: storage}
}

// Storage returns the underlying Storage.
func (s *Service) Storage() *Storage {
	return s.storage
}

// GetEffectiveOnboarding resolves the effective OnboardingSettings by querying
// Global, Project, and User scopes in storage and cascading closest-wins.
func (s *Service) GetEffectiveOnboarding(ctx context.Context, projectID, userID string) (OnboardingSettings, error) {
	return s.storage.ResolveEffectiveOnboarding(ctx, projectID, userID)
}

// SetOnboarding saves typed onboarding settings for a specific scope.
func (s *Service) SetOnboarding(ctx context.Context, scope Scope, scopeID string, ob OnboardingSettings) error {
	if !ValidScope(scope) {
		return fmt.Errorf("invalid settings scope %q", scope)
	}
	return s.storage.SetOnboarding(ctx, scope, scopeID, ob)
}

// GetOnboarding retrieves typed onboarding settings explicitly configured at the given scope.
func (s *Service) GetOnboarding(ctx context.Context, scope Scope, scopeID string) (*OnboardingSettings, error) {
	if !ValidScope(scope) {
		return nil, fmt.Errorf("invalid settings scope %q", scope)
	}
	return s.storage.GetOnboarding(ctx, scope, scopeID)
}

// SetSetting stores or updates a generic setting entry.
func (s *Service) SetSetting(ctx context.Context, setting Setting) error {
	return s.storage.Set(ctx, setting)
}

// GetSetting returns a setting entry by scope, scope_id, and key.
func (s *Service) GetSetting(ctx context.Context, scope Scope, scopeID, key string) (*Setting, error) {
	if !ValidScope(scope) {
		return nil, fmt.Errorf("invalid settings scope %q", scope)
	}
	return s.storage.Get(ctx, scope, scopeID, key)
}

// ListSettings returns all settings configured at a given scope.
func (s *Service) ListSettings(ctx context.Context, scope Scope, scopeID string) ([]Setting, error) {
	if !ValidScope(scope) {
		return nil, fmt.Errorf("invalid settings scope %q", scope)
	}
	return s.storage.ListByScope(ctx, scope, scopeID)
}

// DeleteSetting deletes a single setting.
func (s *Service) DeleteSetting(ctx context.Context, scope Scope, scopeID, key string) error {
	if !ValidScope(scope) {
		return fmt.Errorf("invalid settings scope %q", scope)
	}
	return s.storage.Delete(ctx, scope, scopeID, key)
}
