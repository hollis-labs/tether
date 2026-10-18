//go:build !windows

package agentevents

import (
	"context"
	"errors"
	"os"
	"syscall"
	"time"
)

// lockFile takes an exclusive advisory lock on f; the returned func releases it.
func lockFile(f *os.File) (func(), error) {
	fd := int(f.Fd()) //nolint:gosec // G115: fd fits int
	if err := syscall.Flock(fd, syscall.LOCK_EX); err != nil {
		return nil, err
	}
	return func() { _ = syscall.Flock(fd, syscall.LOCK_UN) }, nil
}

// tryLockFile is lockFile without waiting: it fails when another process holds the lock.
func tryLockFile(f *os.File) (func(), error) {
	fd := int(f.Fd()) //nolint:gosec // G115: fd fits int
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, err
	}
	return func() { _ = syscall.Flock(fd, syscall.LOCK_UN) }, nil
}

// lockPoll is how often lockFileCtx retries a lock another process holds.
const lockPoll = 2 * time.Millisecond

// lockFileCtx is lockFile that gives up when ctx ends, returning ctx.Err(). It polls with a non-blocking lock instead of
// blocking in the syscall, so a writer whose caller has stopped waiting stops too, rather than taking the lock whenever its
// holder lets go and writing into a spool nobody is watching any more.
func lockFileCtx(ctx context.Context, f *os.File) (func(), error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		unlock, err := tryLockFile(f)
		if err == nil {
			return unlock, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(lockPoll):
		}
	}
}
