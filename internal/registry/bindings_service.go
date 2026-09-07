package registry

import (
	"context"
	"fmt"
	"time"
)

// LeaseBinding is the Service-level entry point for claiming current
// ownership of targetURN. See bindings.go for the generation-fencing
// semantics.
func (s *Service) LeaseBinding(ctx context.Context, targetURN, sessionID, hostID, attemptID string, capabilities []string, visibility PublicationVisibility, ttl time.Duration) (RuntimeBinding, error) {
	if targetURN == "" {
		return RuntimeBinding{}, fmt.Errorf("registry: lease binding: %w: target_urn required", ErrInvalidRequest)
	}
	return s.storage.LeaseBinding(ctx, targetURN, sessionID, hostID, attemptID, capabilities, visibility, ttl)
}

// LeaseBindingUnlessVisibility is the Service-level entry point for an
// atomically guarded lease. See bindings.go.
func (s *Service) LeaseBindingUnlessVisibility(ctx context.Context, targetURN, sessionID, hostID, attemptID string, capabilities []string, visibility PublicationVisibility, ttl time.Duration, blocked ...PublicationVisibility) (RuntimeBinding, error) {
	if targetURN == "" {
		return RuntimeBinding{}, fmt.Errorf("registry: lease binding: %w: target_urn required", ErrInvalidRequest)
	}
	return s.storage.LeaseBindingUnlessVisibility(ctx, targetURN, sessionID, hostID, attemptID, capabilities, visibility, ttl, blocked...)
}

// RenewBindingLease extends an existing binding's lease. See bindings.go.
func (s *Service) RenewBindingLease(ctx context.Context, bindingID string, ttl time.Duration) (RuntimeBinding, error) {
	if bindingID == "" {
		return RuntimeBinding{}, fmt.Errorf("registry: renew binding lease: %w: binding id required", ErrInvalidRequest)
	}
	return s.storage.RenewLease(ctx, bindingID, ttl)
}

// RevokeBinding relinquishes a lease. See bindings.go.
func (s *Service) RevokeBinding(ctx context.Context, bindingID string) error {
	if bindingID == "" {
		return fmt.Errorf("registry: revoke binding: %w: binding id required", ErrInvalidRequest)
	}
	return s.storage.RevokeBinding(ctx, bindingID)
}

// CurrentBinding returns the single authoritative binding for targetURN, if
// one exists. See bindings.go.
func (s *Service) CurrentBinding(ctx context.Context, targetURN string) (RuntimeBinding, error) {
	if targetURN == "" {
		return RuntimeBinding{}, fmt.Errorf("registry: current binding: %w: target_urn required", ErrInvalidRequest)
	}
	return s.storage.CurrentBinding(ctx, targetURN)
}

// ListBindingsForTarget returns every binding ever leased for targetURN.
func (s *Service) ListBindingsForTarget(ctx context.Context, targetURN string) ([]RuntimeBinding, error) {
	if targetURN == "" {
		return nil, fmt.Errorf("registry: list bindings: %w: target_urn required", ErrInvalidRequest)
	}
	return s.storage.ListBindingsForTarget(ctx, targetURN)
}
