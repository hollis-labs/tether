//go:build !windows

package app

import (
	"context"
	"errors"
	"syscall"

	"github.com/hollis-labs/tether/internal/shimhost"
	"github.com/hollis-labs/tether/internal/store"
)

func recordedPIDAbsent(pid int) bool {
	return pid > 0 && errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}

// recoverGoneTeamShim consumes only the confirmed-Gone branch. The launch
// lock excludes same-ID admission until exact old custody is retired. Current
// PID absence is deliberately conservative: zero, permission errors and live
// or reused PIDs are unavailable, never authorization to signal a process.
func (s *Service) recoverGoneTeamShim(ctx context.Context, row store.SessionShimRow, receipt shimhost.Receipt, host *shimHosting, absent func(int) bool) error {
	unlock, err := s.lockSessionLaunch(ctx, row.SessionID)
	if err != nil {
		return err
	}
	defer unlock()
	if _, live := s.Manager.Get(row.SessionID); live {
		return store.ErrTeamShimRecoveryUnavailable
	}
	fence, err := s.Store.GoneTeamShimRecoveryFence(ctx, row)
	if err != nil {
		return err
	}
	canonical, err := loadShimReceipt(row)
	if err != nil || canonical != receipt {
		return store.ErrTeamShimRecoveryUnavailable
	}
	if !absent(canonical.HostPID) || !absent(canonical.ProviderPID) {
		return store.ErrTeamShimRecoveryUnavailable
	}
	if err = host.stopProvider(ctx, canonical); err != nil {
		return err
	}
	retired, err := loadShimReceipt(row)
	if err != nil || !retired.Retired {
		return store.ErrTeamShimRecoveryUnavailable
	}
	// Retirement may update its flag, but must not change the captured placement
	// or its process identity while we are deciding which custody can be cleared.
	retired.Retired = canonical.Retired
	if retired != canonical || !absent(retired.HostPID) || !absent(retired.ProviderPID) {
		return store.ErrTeamShimRecoveryUnavailable
	}
	change, err := s.Store.CommitGoneTeamShimRecovery(ctx, fence)
	if err != nil {
		return err
	}
	s.publishSessionStateChange(change)
	s.shimDiagnostic(row.SessionID, &canonical, "recovery_pending", "confirmed_gone")
	return nil
}
