//go:build !linux && !windows

package shimhost

import (
	"context"
	"net"
	"syscall"
)

type processHandle struct{ pid int }

func peerPID(_ *net.UnixConn) (int, error) {
	return 0, fail("unsupported", "socket peer pidfds require Linux")
}
func authenticatedProcess(_ *Client, _ int) (*processHandle, error) {
	return nil, fail("unsupported", "pidfd-bound host teardown requires Linux")
}
func (p *processHandle) close() {}
func (p *processHandle) signal(_ syscall.Signal) error {
	return fail("unsupported", "pidfd-bound host teardown requires Linux")
}
func (p *processHandle) wait(_ context.Context) error {
	return fail("unsupported", "pidfd-bound host teardown requires Linux")
}

func processStartTime(int) (uint64, error) {
	return 0, fail("unsupported", "host identity requires Linux")
}
func peerStartTime(*processHandle) (uint64, error) {
	return 0, fail("unsupported", "host identity requires Linux")
}
func recordedIdentityGone(int, uint64) bool { return false }

// The owned waiter proves this submitted child exited without Linux pidfds.
func reapedIdentityGone(int, uint64) bool { return true }
