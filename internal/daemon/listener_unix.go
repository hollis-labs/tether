//go:build unix

package daemon

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"time"
)

// removeStaleSocket deletes the file at path only if it is a Unix socket
// AND nothing is actually listening on it.
//
// The second check (added by T11's durability review, CW-20260906-0042)
// closes a live-reproduced split-brain: a socket-typed file existing was
// previously treated as sufficient proof of staleness, relying entirely
// on daemon.Server.Run's separate PID-file liveness check to catch a
// still-live daemon. A PID file is not a reliable liveness signal on its
// own -- it can be deleted, corrupted, or simply absent (an external
// cleanup script, a disk hiccup, a race with WritePIDFile) while the
// daemon that owns this socket is still genuinely running. Reproduced
// exactly that: deleting a live daemon's PID file let a second `daemon
// run` invocation delete its socket file out from under it and bind a
// fresh one, producing two fully independent, fully live daemon
// processes against the same state.db simultaneously. A real connect
// attempt is a second, independent liveness signal that doesn't depend
// on the PID file being intact at all.
func removeStaleSocket(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("listen_addr path %q exists and is not a socket", path)
	}
	if conn, dialErr := net.DialTimeout("unix", path, 200*time.Millisecond); dialErr == nil {
		_ = conn.Close()
		return fmt.Errorf("%w: a process is still accepting connections on %q", ErrAlreadyRunning, path)
	}
	return os.Remove(path)
}
