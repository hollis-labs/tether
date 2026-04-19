package daemon

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// ErrAlreadyRunning is returned when a PID file exists and points at a
// process that responds to signal 0 (i.e., another daemon is live).
var ErrAlreadyRunning = errors.New("daemon already running")

// WritePIDFile writes pid to path, creating parent directories as needed
// and refusing to overwrite a file that points at a currently-live
// process. A stale PID file (referencing a dead or unrelated process) is
// overwritten.
func WritePIDFile(path string, pid int) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir pidfile dir: %w", err)
	}
	if existing, err := ReadPIDFile(path); err == nil {
		if existing == pid {
			// same process re-writing; tolerate.
		} else if IsAlive(existing) {
			return fmt.Errorf("%w (pid %d)", ErrAlreadyRunning, existing)
		}
	}
	return os.WriteFile(path, []byte(strconv.Itoa(pid)+"\n"), 0o644)
}

// ReadPIDFile reads a PID from path. Returns fs.ErrNotExist if missing.
// Returns an error if the contents are not a valid integer.
func ReadPIDFile(path string) (int, error) {
	b, err := os.ReadFile(path)
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
