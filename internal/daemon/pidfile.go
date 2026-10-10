package daemon

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	localdaemon "github.com/hollis-labs/libs/util/localdaemon"
)

// ErrAlreadyRunning is returned when a PID file exists and points at a
// process that responds to signal 0 (i.e., another daemon is live).
var ErrAlreadyRunning = errors.New("daemon already running")

// WritePIDFile writes pid to path, creating parent directories as needed
// and refusing to overwrite a file that points at a currently-live
// process. A stale PID file (referencing a dead or unrelated process) is
// overwritten.
func WritePIDFile(path string, pid int) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("mkdir pidfile dir: %w", err)
	}
	if existing, err := ReadPIDFile(path); err == nil {
		if existing == pid {
			// same process re-writing; tolerate.
		} else if IsDaemonAlive(existing) {
			return fmt.Errorf("%w (pid %d)", ErrAlreadyRunning, existing)
		}
	}
	return os.WriteFile(path, []byte(strconv.Itoa(pid)+"\n"), 0o600)
}

// ReadPIDFile reads a PID from path. Returns fs.ErrNotExist if missing.
// Returns an error if the contents are not a valid integer.
func ReadPIDFile(path string) (int, error) {
	// path is a catalog-configured pidfile location under the user's own
	// data dir; not user input from an HTTP boundary.
	b, err := os.ReadFile(path) //nolint:gosec // G304: catalog-sourced path, not untrusted input

	if err != nil {
		return 0, err
	}
	s := strings.TrimSpace(string(b))
	pid, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("parse pidfile %q: %w", path, err)
	}
	return pid, nil
}

// RemovePIDFile deletes the PID file at path. Missing file is not an error.
func RemovePIDFile(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// IsAlive reports whether a process with pid is currently running. Uses
// signal 0 (POSIX); on Windows it falls back to os.FindProcess semantics.
func IsAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

// daemonCmdRegex recognizes the foreground daemon entry points, including known
// root flags before the subcommand. Anchor at the executable so a provider or
// shell carrying a tether command as an argument is not mistaken for tetherd.
var daemonCmdRegex = regexp.MustCompile(`^(?:[^\s]+/)?tether[^\s/]*\s+(?:--(?:catalog|token-file)(?:=[^\s]*|\s+[^\s]+)\s+)*(?:daemon\s+run|serve)(?:\s|$)`)

// verifyCommand is localdaemon.VerifyCommand; tests replace it.
var verifyCommand = localdaemon.VerifyCommand

// IsDaemon reports whether pid is running as a tetherd, by reading its command
// line. IsAlive only says some process holds that PID, which after a crash
// (the PID file is removed only on graceful shutdown) may be an unrelated
// process that was handed the recycled number. A nonexistent or non-matching
// PID is (false, nil); an error means the check itself could not be made, and
// a caller must not signal on it.
func IsDaemon(ctx context.Context, pid int) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return verifyCommand(ctx, pid, daemonCmdRegex)
}

// IsDaemonAlive reports whether pid is a live tetherd, for guards that refuse to
// start a second daemon or report status. A live PID that is provably not tetherd
// is stale. When the identity check cannot be made it answers true, so a guard
// errs toward "already running" and never toward starting over a live daemon.
// Do not use it to decide whether to signal: use IsDaemon.
func IsDaemonAlive(pid int) bool {
	if !IsAlive(pid) {
		return false
	}
	ok, err := IsDaemon(context.Background(), pid)
	return ok || err != nil
}
