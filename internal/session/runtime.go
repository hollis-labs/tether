package session

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"

	"github.com/creack/pty"
)

// Handle owns a PTY-spawned subprocess plus its log + done plumbing.
//
// PTY-touching methods (Resize, Write, PTYWriter) take a read-lock; the
// wait goroutine takes a write-lock to swap PTY → nil before calling
// Close. This serializes the FD-destroy / FD-use pair that previously
// raced when Resize called pty.Setsize while the wait goroutine closed
// the master fd after cmd.Wait() returned.
type Handle struct {
	Cmd *exec.Cmd
	// PTY is the master fd. Writers go through Write; readers through
	// the io.Copy goroutine spawned in Start. It is nil after the wait
	// goroutine has cleared it (post-cmd.Wait).
	PTY     *os.File
	LogFile *os.File

	ptyMu sync.RWMutex

	done     chan error
	waitOnce sync.Once
	waitCode int
	waitErr  error
}

// errPTYClosed is the sentinel returned by Resize / Write when the PTY
// has been cleared by the wait goroutine. Callers map it to whatever
// "session input channel closed" error their layer uses.
var errPTYClosed = errors.New("session pty is closed")

// Start launches cmd under a PTY. Stdout/stderr are mirrored to logPath.
// bootPrompt, if non-empty and bootMode=="stdin", is written to the PTY before
// returning so the child process sees it as initial input.
//
// If fanout is non-nil, PTY bytes are also written to it as they arrive. The
// runtime layer uses this to publish live output to attach subscribers; the
// fanout writer is never closed by Start (the caller owns its lifecycle).
func Start(cmd *exec.Cmd, logPath, bootPrompt, bootMode string, fanout io.Writer) (*Handle, error) {
	// logPath is workspace-owned (derived from catalog + session id), not user input.
	logF, err := os.Create(logPath) //nolint:gosec // G304: workspace-managed path

	if err != nil {
		return nil, fmt.Errorf("open log: %w", err)
	}
	ptmx, err := pty.Start(cmd)
	if err != nil {
		logF.Close()
		return nil, fmt.Errorf("pty start: %w", err)
	}
	h := &Handle{Cmd: cmd, PTY: ptmx, LogFile: logF, done: make(chan error, 1)}

	if bootMode == "stdin" && bootPrompt != "" {
		if _, err := io.WriteString(ptmx, bootPrompt); err != nil {
			_ = cmd.Process.Kill()
			ptmx.Close()
			logF.Close()
			return nil, fmt.Errorf("write boot prompt: %w", err)
		}
	}

	var sink io.Writer = logF
	if fanout != nil {
		sink = io.MultiWriter(logF, fanout)
	}

	copyDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(sink, ptmx)
		close(copyDone)
	}()
	go func() {
		err := cmd.Wait()

		// Clear the PTY pointer under the write lock so any in-flight
		// Resize / Write returns errPTYClosed before the FD-destroying
		// Close() runs. We close the captured local outside the lock —
		// Close blocks until io.Copy unwinds, and we don't want to hold
		// the lock across that.
		h.ptyMu.Lock()
		h.PTY = nil
		h.ptyMu.Unlock()

		ptmx.Close() // unblocks io.Copy
		<-copyDone   // wait for final bytes to drain into logF + fanout
		logF.Close()
		h.done <- err
	}()
	return h, nil
}

// Wait blocks until the process exits; returns exit code (0 on success, -1 if signaled).
// Safe for multiple callers: the first call consumes the done channel, subsequent
// calls return the cached result.
func (h *Handle) Wait() (int, error) {
	h.waitOnce.Do(func() {
		err := <-h.done
		if err == nil {
			h.waitCode = 0
			return
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			h.waitCode = ee.ExitCode()
			return
		}
		h.waitCode = -1
		h.waitErr = err
	})
	return h.waitCode, h.waitErr
}

// Kill terminates the process.
func (h *Handle) Kill() error {
	if h.Cmd.Process == nil {
		return nil
	}
	return h.Cmd.Process.Kill()
}

// PID returns the child process PID, or 0 if the process is not started.
func (h *Handle) PID() int {
	if h.Cmd == nil || h.Cmd.Process == nil {
		return 0
	}
	return h.Cmd.Process.Pid
}

// Write injects bytes into the PTY master under the read lock. Returns
// errPTYClosed if the wait goroutine has already cleared the PTY pointer.
// Concurrent callers are safe — the underlying *os.File serializes
// internally; the lock here is only against the destroy-during-use race
// with the wait goroutine.
func (h *Handle) Write(p []byte) (int, error) {
	h.ptyMu.RLock()
	defer h.ptyMu.RUnlock()
	if h.PTY == nil {
		return 0, errPTYClosed
	}
	return h.PTY.Write(p)
}

// IsPTYClosed reports whether err is the closed-PTY sentinel returned by
// Write / Resize when the PTY has been cleared. Callers map it to the
// agentsessions.ErrNoInputChannel boundary.
func IsPTYClosed(err error) bool {
	return errors.Is(err, errPTYClosed)
}

// PTYWriter returns a writer wrapping the PTY master with the same lock
// discipline as Handle.Write. Useful for callers that need an io.Writer
// for io.Copy-style plumbing; for one-shot input delivery prefer
// Handle.Write directly.
func (h *Handle) PTYWriter() io.Writer {
	h.ptyMu.RLock()
	defer h.ptyMu.RUnlock()
	if h.PTY == nil {
		return nil
	}
	return ptyWriter{h: h}
}

type ptyWriter struct{ h *Handle }

func (w ptyWriter) Write(p []byte) (int, error) { return w.h.Write(p) }

// Resize updates the PTY's winsize to (rows, cols). The child process
// typically observes this as a SIGWINCH + a re-read of TIOCGWINSZ —
// full-screen TUI apps (vim, top, claude CLI) redraw at the new shape.
// Returns nil on success; errPTYClosed if the PTY has been cleared, or
// the underlying ioctl error otherwise.
//
// Held under read-lock so it cannot race with the wait goroutine's
// pointer-clear / Close pair.
func (h *Handle) Resize(rows, cols uint16) error {
	h.ptyMu.RLock()
	defer h.ptyMu.RUnlock()
	if h.PTY == nil {
		return errPTYClosed
	}
	return pty.Setsize(h.PTY, &pty.Winsize{Rows: rows, Cols: cols})
}
