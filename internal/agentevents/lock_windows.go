//go:build windows

package agentevents

import "os"

// lockFile is a no-op on Windows: the spool's O_APPEND writes are atomic per call, and the per-session sequence there is
// only as strong as a single writer. The agent adapters this serves run on unix hosts.
func lockFile(_ *os.File) (func(), error) { return func() {}, nil }

// tryLockFile is a no-op on Windows (see lockFile).
func tryLockFile(_ *os.File) (func(), error) { return func() {}, nil }
