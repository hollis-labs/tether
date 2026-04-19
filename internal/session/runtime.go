package session

import (
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
func Start(cmd *exec.Cmd, logPath, bootPrompt, bootMode string) (*Handle, error) {
	logF, err := os.Create(logPath)
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

	copyDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(logF, ptmx)
		close(copyDone)
	}()
	go func() {
		err := cmd.Wait()
		ptmx.Close() // unblocks io.Copy
		<-copyDone   // wait for final bytes to drain into logF
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
		switch {
		case err == nil:
			h.waitCode = 0
		default:
			if ee, ok := err.(*exec.ExitError); ok {
				h.waitCode = ee.ExitCode()
			} else {
				h.waitCode = -1
				h.waitErr = err
			}
		}
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

// Attach copies the log file from start to current EOF into w.
// v0 is snapshot-only; live-follow ("tail -f") is deferred to a later task.
func (h *Handle) Attach(w io.Writer) error {
	f, err := os.Open(h.LogFile.Name())
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(w, f)
	return err
}
