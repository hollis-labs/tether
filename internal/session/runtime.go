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

type Handle struct {
	Cmd     *exec.Cmd
	PTY     *os.File
	LogFile *os.File

	done     chan error
	waitOnce sync.Once
	waitCode int
	waitErr  error
}

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

// PTYWriter returns the PTY master so callers can inject input into the
// running session. Returns nil if the session has already closed its PTY.
// Writes to this writer are not guarded; concurrent callers must serialize
// through runtime.Manager.SendInput which holds the per-session lock.
func (h *Handle) PTYWriter() io.Writer {
	if h.PTY == nil {
		return nil
	}
	return h.PTY
}
