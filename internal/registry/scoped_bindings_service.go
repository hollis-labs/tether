package registry

import (
	"context"
	"encoding/json"
	"fmt"
)

// SetScopedBinding is the Service-level entry point for publishing a new
// revision of a scope/slot resolution. See scoped_bindings.go.
func (s *Service) SetScopedBinding(ctx context.Context, scope, slot string, targetURNs []string, relationship json.RawMessage, createdBy string) (ScopedBinding, error) {
	if scope == "" || slot == "" {
		return ScopedBinding{}, fmt.Errorf("registry: set scoped binding: %w: scope and slot are required", ErrInvalidRequest)
	}
	if createdBy == "" {
		return ScopedBinding{}, fmt.Errorf("registry: set scoped binding: %w: created_by is required for provenance", ErrInvalidRequest)
	}
	for _, urn := range targetURNs {
		if _, err := ParseRegistryURN(urn); err != nil {
			return ScopedBinding{}, fmt.Errorf("registry: set scoped binding: %w: invalid target URN %q: %w", ErrInvalidRequest, urn, err)
		}
	}
	return s.storage.SetScopedBinding(ctx, scope, slot, targetURNs, relationship, createdBy)
}

// ResolveScopedBinding returns the current revision for (scope, slot).
func (s *Service) ResolveScopedBinding(ctx context.Context, scope, slot string) (ScopedBinding, error) {
	if scope == "" || slot == "" {
		return ScopedBinding{}, fmt.Errorf("registry: resolve scoped binding: %w: scope and slot are required", ErrInvalidRequest)
	}
	return s.storage.ResolveScopedBinding(ctx, scope, slot)
}

// ResolveScopedBindingSingle resolves (scope, slot) to exactly one target,
// erroring on zero or multiple targets. See scoped_bindings.go.
func (s *Service) ResolveScopedBindingSingle(ctx context.Context, scope, slot string) (string, ScopedBinding, error) {
	if scope == "" || slot == "" {
		return "", ScopedBinding{}, fmt.Errorf("registry: resolve scoped binding: %w: scope and slot are required", ErrInvalidRequest)
	}
	return s.storage.ResolveScopedBindingSingle(ctx, scope, slot)
}

// ListScopedBindingRevisions returns the full provenance history for
// (scope, slot), newest first.
func (s *Service) ListScopedBindingRevisions(ctx context.Context, scope, slot string) ([]ScopedBinding, error) {
	if scope == "" || slot == "" {
		return nil, fmt.Errorf("registry: list scoped binding revisions: %w: scope and slot are required", ErrInvalidRequest)
	}
	return s.storage.ListScopedBindingRevisions(ctx, scope, slot)
}
