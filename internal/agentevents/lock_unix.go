//go:build !windows

package agentevents

import (
	"os"
	"syscall"
)

// lockFile takes an exclusive advisory lock on f; the returned func releases it.
func lockFile(f *os.File) (func(), error) {
	fd := int(f.Fd()) //nolint:gosec // G115: fd fits int
	if err := syscall.Flock(fd, syscall.LOCK_EX); err != nil {
		return nil, err
	}
	return func() { _ = syscall.Flock(fd, syscall.LOCK_UN) }, nil
}
