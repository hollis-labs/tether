//go:build linux

package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

type processAbsent struct{ boot string }

func (p *processAbsent) Error() string { return "process incarnation is absent" }

func processIdentity(pid int) (lockOwner, error) {
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return lockOwner{}, err
	}
	if !bootIdentity.MatchString(strings.TrimSpace(string(boot))) {
		return lockOwner{}, fmt.Errorf("boot identity is unavailable")
	}
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		if os.IsNotExist(err) {
			return lockOwner{}, &processAbsent{boot: strings.TrimSpace(string(boot))}
		}
		return lockOwner{}, err
	}
	end := strings.LastIndex(string(stat), ")")
	if end < 0 {
		return lockOwner{}, fmt.Errorf("invalid process identity")
	}
	fields := strings.Fields(string(stat)[end+1:])
	if len(fields) <= 19 {
		return lockOwner{}, fmt.Errorf("incomplete process identity")
	}
	if _, err := strconv.ParseUint(fields[19], 10, 64); err != nil {
		return lockOwner{}, err
	}
	return lockOwner{PID: pid, Boot: strings.TrimSpace(string(boot)), Start: fields[19]}, nil
}

func ownerAlive(owner lockOwner) (bool, error) {
	identity, err := processIdentity(owner.PID)
	if err != nil {
		// Missing boot metadata, access errors and a single ESRCH during
		// reap are unknown. Require two matching boot/proc-absence samples
		// with positive kernel absence before reclaiming a dead incarnation.
		var first *processAbsent
		if errors.As(err, &first) && errors.Is(unix.Kill(owner.PID, 0), unix.ESRCH) {
			_, secondErr := processIdentity(owner.PID)
			var second *processAbsent
			if errors.As(secondErr, &second) && first.boot == second.boot && errors.Is(unix.Kill(owner.PID, 0), unix.ESRCH) {
				return false, nil
			}
		}
		return false, err
	}
	err = unix.Kill(owner.PID, 0)
	if err == nil || errors.Is(err, unix.EPERM) {
		return identity.Boot == owner.Boot && identity.Start == owner.Start, nil
	}
	if errors.Is(err, unix.ESRCH) {
		return false, fmt.Errorf("process changed during liveness inspection")
	}
	return false, err
}

func lockGuard(ctx context.Context, path string) (func(), error) {
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("kernel guard must be a regular file")
	}
	for {
		if err := ctx.Err(); err != nil {
			_ = unix.Close(fd)
			return nil, err
		}
		err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			_ = unix.Close(fd)
			return nil, err
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
	return func() { _ = unix.Flock(fd, unix.LOCK_UN); _ = unix.Close(fd) }, nil
}
