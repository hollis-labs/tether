//go:build !windows

package app

import (
	"context"

	"github.com/hollis-labs/tether/internal/shimhost"
	"github.com/hollis-labs/tether/internal/shimretire"
	"github.com/hollis-labs/tether/internal/store"
)

// AcquireShimRetirementFence is an internal composition seam, not an operator
// route. Complete controller/journal exclusion and backend drain/containment
// evidence are additional prerequisites; sessionLaunchGate alone is insufficient.
func (s *Service) AcquireShimRetirementFence(ctx context.Context, receipt shimhost.Receipt, operation string, excludeControllers func(context.Context) (func(), error)) (*shimhost.PlacementFence, func(), error) {
	if excludeControllers == nil {
		return nil, nil, &shimhost.Failure{Code: "unsupported", Message: "complete controller exclusion unavailable"}
	}
	release, err := s.lockSessionLaunch(ctx, receipt.Session)
	if err != nil {
		return nil, nil, err
	}
	releaseControllers, err := excludeControllers(ctx)
	if err != nil || releaseControllers == nil {
		if releaseControllers != nil {
			releaseControllers()
		}
		release()
		return nil, nil, &shimhost.Failure{Code: "unsupported", Message: "complete controller exclusion unavailable"}
	}
	releaseAll := func() { releaseControllers(); release() }
	host, err := s.shimHost()
	if err != nil {
		releaseAll()
		return nil, nil, err
	}
	fence, err := host.provider.AcquireRetirementFence(ctx, receipt, operation)
	if err != nil {
		releaseAll()
		return nil, nil, err
	}
	return fence, releaseAll, nil
}

// ReconcileShimRetirement is consumed only by a fully proven retirement lease.
// No public/runtime caller is wired while real complete proof is Unsupported.
func (s *Service) ReconcileShimRetirement(ctx context.Context, placement shimretire.Placement, operation string) error {
	adapter := store.ShimRetirementStore{Store: s.Store}
	change, err := adapter.ReconcileRetirement(ctx, placement, operation)
	if err != nil {
		return err
	}
	s.publishSessionStateChange(change)
	return nil
}
