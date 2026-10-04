//go:build linux

package shimhost

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// getPeerPIDFD is the kernel capability seam for pre-hello teardown checks.
var getPeerPIDFD = func(fd int) (int, error) {
	return unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_PEERPIDFD)
}

// Supported probes peer pidfds without starting a host or taking a controller.
func Supported() error {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return fail("unsupported", "cannot probe socket peer pidfds")
	}
	defer func() { _ = unix.Close(fds[0]); _ = unix.Close(fds[1]) }()
	fd, err := getPeerPIDFD(fds[0])
	if err != nil {
		return fail("unsupported", "kernel must support socket peer pidfds")
	}
	_ = unix.Close(fd)
	return nil
}

type processHandle struct {
	fd  int
	pid int
}

func peerPID(socket *net.UnixConn) (int, error) {
	raw, err := socket.SyscallConn()
	if err != nil {
		return 0, err
	}
	var pid int
	var inner error
	err = raw.Control(func(fd uintptr) {
		cred, e := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		inner = e
		if e == nil {
			if int64(cred.Uid) != int64(os.Getuid()) {
				inner = fail("identity_mismatch", "foreign host uid")
				return
			}
			pid = int(cred.Pid)
		}
	})
	if err != nil {
		return 0, err
	}
	return pid, inner
}
func authenticatedProcess(c *Client, expectedPID int) (*processHandle, error) {
	pid, err := peerPID(c.socket)
	if err != nil {
		return nil, err
	}
	if pid <= 0 || expectedPID > 0 && expectedPID != pid {
		return nil, fail("identity_mismatch", "same-uid peer PID differs from recorded host")
	}
	raw, err := c.socket.SyscallConn()
	if err != nil {
		return nil, err
	}
	fd := -1
	var inner error
	err = raw.Control(func(socketFD uintptr) {
		fd, inner = getPeerPIDFD(int(socketFD))
	})
	if err != nil {
		return nil, err
	}
	if errors.Is(inner, unix.ESRCH) {
		return nil, unix.ESRCH
	}
	if inner != nil {
		return nil, fail("unsupported", "kernel must support socket peer pidfds")
	}
	if fd < 0 || fd > math.MaxInt32 {
		if fd >= 0 {
			_ = unix.Close(fd)
		}
		return nil, fail("unsupported", "invalid host pidfd")
	}
	unix.CloseOnExec(fd)
	return &processHandle{fd: fd, pid: pid}, nil
}
func (p *processHandle) close() { _ = unix.Close(p.fd) }
func (p *processHandle) signal(signal unix.Signal) error {
	err := unix.PidfdSendSignal(p.fd, signal, nil, 0)
	if errors.Is(err, unix.ESRCH) {
		return nil
	}
	return err
}
func (p *processHandle) wait(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		fds := []unix.PollFd{{Fd: int32(p.fd), Events: unix.POLLIN}} //nolint:gosec // Descriptor range checked when acquired.
		_, err := unix.Poll(fds, 10)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return err
		}
		if fds[0].Revents&unix.POLLIN != 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
}

// readProcessStat is the process-identity source; state and start time come from
// the same read, including when an exited child has not yet been reaped.
var readProcessStat = func(pid int) ([]byte, error) { return os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)) }

// processStartTime is the Linux process identity witness, not a liveness test.
func processStartTime(pid int) (uint64, error) {
	start, _, err := processIdentity(pid)
	return start, err
}
func processIdentity(pid int) (uint64, string, error) {
	raw, err := readProcessStat(pid)
	if err != nil {
		return 0, "", err
	}
	end := strings.LastIndexByte(string(raw), ')')
	if end < 0 {
		return 0, "", fail("outcome_unknown", "host identity unavailable")
	}
	fields := strings.Fields(string(raw[end+1:]))
	if len(fields) < 20 {
		return 0, "", fail("outcome_unknown", "host identity unavailable")
	}
	start, err := strconv.ParseUint(fields[19], 10, 64)
	return start, fields[0], err
}
func peerStartTime(p *processHandle) (uint64, error) {
	if err := unix.PidfdSendSignal(p.fd, 0, nil, 0); err != nil {
		return 0, err
	}
	start, err := processStartTime(p.pid)
	if err != nil {
		return 0, err
	}
	if err := unix.PidfdSendSignal(p.fd, 0, nil, 0); err != nil {
		return 0, err
	}
	return start, nil
}
func recordedIdentityGone(pid int, start uint64) bool {
	if pid <= 0 || start == 0 {
		return false
	}
	// An absent or unreadable proc filesystem is not process absence.
	if _, err := processStartTime(os.Getpid()); err != nil {
		return false
	}
	current, state, err := processIdentity(pid)
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	if err != nil {
		return false
	}
	if state == "Z" || state == "X" {
		return current == start
	}
	return current != start
}

func reapedIdentityGone(pid int, start uint64) bool { return recordedIdentityGone(pid, start) }
