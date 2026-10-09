//go:build linux

package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/hollis-labs/tether/internal/launchprofile"
	"github.com/hollis-labs/tether/internal/store"
	"golang.org/x/sys/unix"
)

// pidfd pins the verified live process across all stop tiers. We never signal
// a bare persisted PID, which could belong to another process after reuse.
func (s *Service) stopVerifiedSessionProcess(ctx context.Context, row *store.SessionRow, pid int, reason string, d launchprofile.LifecycleDurations) error {
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return fmt.Errorf("pin session process: %w", err)
	}
	defer func() { _ = unix.Close(fd) }()
	var recorded string
	if err = s.Store.DB().QueryRowContext(ctx, `SELECT COALESCE(pid_started_at,'') FROM sessions WHERE id=? AND pid=?`, row.ID, pid).Scan(&recorded); err != nil {
		return err
	}
	actual, known := osProcessInspector{}.startTime(pid)
	if recorded == "" || !known || recorded != actual {
		return fmt.Errorf("session process identity unverified; retained")
	}
	pgid, err := unix.Getpgid(pid)
	if err != nil {
		return err
	}
	if pgid != pid {
		return s.finishLifecycleRuntimeStop(ctx, row.ID, d)
	}
	flags := unix.PIDFD_SIGNAL_PROCESS_GROUP
	if err = unix.PidfdSendSignal(fd, 0, nil, flags); errors.Is(err, syscall.EINVAL) {
		// Older kernels lack pinned process-group signaling. Keep teardown in the
		// owning runtime rather than opening a raw-PID reuse race.
		if err := s.reaperEvent(ctx, row, reason, "runtime_fallback", "", nil); err != nil {
			return err
		}
		return s.finishLifecycleRuntimeStop(ctx, row.ID, d)
	}
	if err = unix.PidfdSendSignal(fd, 0, nil, flags); errors.Is(err, syscall.ESRCH) {
		return nil
	} else if err != nil {
		return err
	}
	// SIGINT is a cooperative stop request; TERM and KILL follow only after
	// their own grace expires. WaitSession observes the runtime's real exit.
	for _, tier := range []struct {
		name   string
		signal syscall.Signal
		grace  time.Duration
	}{
		{"request_stop", syscall.SIGINT, d.RequestGrace}, {"terminate", syscall.SIGTERM, d.TerminateGrace}, {"kill", syscall.SIGKILL, d.KillGrace},
	} {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err = s.reaperEvent(ctx, row, reason, tier.name, "", nil); err != nil {
			return err
		}
		if err = unix.PidfdSendSignal(fd, tier.signal, nil, flags); errors.Is(err, syscall.ESRCH) {
			return nil
		} else if err != nil {
			return err
		}
		waitCtx, cancel := context.WithTimeout(ctx, tier.grace)
		waitErr := s.waitLifecycleExit(waitCtx, row.ID)
		cancel()
		if waitErr == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !errors.Is(waitErr, context.DeadlineExceeded) {
			return waitErr
		}
	}
	return fmt.Errorf("session did not settle after kill grace; retained")
}

func sessionProcessZombie(pid int) bool {
	// A zombie still answers kill(0), but has no runnable child to retain.
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat") //nolint:gosec // daemon-observed numeric PID, fixed proc path
	if err != nil {
		return false
	}
	_, tail, ok := strings.Cut(string(data), ") ")
	return ok && strings.HasPrefix(tail, "Z ")
}
