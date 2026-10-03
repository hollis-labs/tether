//go:build linux

package shimhost

import (
	"context"
	"errors"
	"math"
	"net"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

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
		return nil, fail("identity_mismatch", "authenticated peer differs from recorded host")
	}
	raw, err := c.socket.SyscallConn()
	if err != nil {
		return nil, err
	}
	fd := -1
	var inner error
	err = raw.Control(func(socketFD uintptr) {
		fd, inner = unix.GetsockoptInt(int(socketFD), unix.SOL_SOCKET, unix.SO_PEERPIDFD)
	})
	if err != nil {
		return nil, err
	}
	if inner != nil {
		return nil, fail("unsupported", "kernel must support authenticated peer pidfds")
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
