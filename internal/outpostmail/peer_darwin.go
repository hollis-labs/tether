//go:build darwin

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
		cred, e := unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
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
