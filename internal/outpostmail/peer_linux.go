//go:build linux

package outpostmail

import (
	"golang.org/x/sys/unix"
	"net"
)

func peerUID(c *net.UnixConn) (int, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return 0, err
	}
	var uid int
	var check error
	err = raw.Control(func(fd uintptr) {
		cred, e := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) //nolint:gosec // G115: fd is a live OS descriptor from SyscallConn.Control, representable as an int
		check = e
		if e == nil {
			uid = int(cred.Uid)
		}
	})
	if err != nil {
		return 0, err
	}
	return uid, check
}
