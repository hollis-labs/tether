//go:build !linux && !windows

package shimhost

import (
	"context"
	"net"
	"syscall"
)

type processHandle struct{ pid int }

func peerPID(_ *net.UnixConn) (int, error) {
	return 0, fail("unsupported", "authenticated peer pidfds require Linux")
}
func authenticatedProcess(_ *Client, _ int) (*processHandle, error) {
	return nil, fail("unsupported", "authenticated host teardown requires Linux")
}
func (p *processHandle) close() {}
func (p *processHandle) signal(_ syscall.Signal) error {
	return fail("unsupported", "authenticated host teardown requires Linux")
}
func (p *processHandle) wait(_ context.Context) error {
	return fail("unsupported", "authenticated host teardown requires Linux")
}
