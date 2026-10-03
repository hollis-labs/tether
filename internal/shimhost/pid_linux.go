//go:build linux

package shimhost

import (
	"golang.org/x/sys/unix"
	"net"
)

func peerPID(path string) (int, error) {
	socket, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return 0, err
	}
	defer func() { _ = socket.Close() }()
	raw, err := socket.SyscallConn()
	if err != nil {
		return 0, err
	}
	var pid int
	var controlErr error
	err = raw.Control(func(fd uintptr) {
		cred, e := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		controlErr = e
		if e == nil {
			pid = int(cred.Pid)
		}
	})
	if err != nil {
		return 0, err
	}
	return pid, controlErr
}
