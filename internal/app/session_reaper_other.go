//go:build !linux

package app

import (
	"context"
	"github.com/hollis-labs/tether/internal/launchprofile"
	"github.com/hollis-labs/tether/internal/store"
)

// Other platforms retain the runtime-owned identity-safe teardown contract.
func (s *Service) stopVerifiedSessionProcess(ctx context.Context, row *store.SessionRow, _ int, _ string, d launchprofile.LifecycleDurations) error {
	return s.finishLifecycleRuntimeStop(ctx, row.ID, d)
}

func sessionProcessZombie(int) bool { return false }
