package session

import (
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/creack/pty"
)

type Handle struct {
	Cmd     *exec.Cmd
	PTY     *os.File
	LogFile *os.File
	done    chan error
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

	go func() {
		_, _ = io.Copy(logF, ptmx)
	}()
	go func() {
		h.done <- cmd.Wait()
		ptmx.Close()
		logF.Close()
	}()
	return h, nil
}

// Wait blocks until the process exits; returns exit code (0 on success, -1 if signaled).
func (h *Handle) Wait() (int, error) {
	err := <-h.done
	if err == nil {
		return 0, nil
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), nil
	}
	return -1, err
}

// Kill terminates the process.
func (h *Handle) Kill() error {
	if h.Cmd.Process == nil {
		return nil
	}
	return h.Cmd.Process.Kill()
}

// Attach copies the log file tail to w, then streams live PTY output.
// For v0: simply tail the log file from start to end.
func (h *Handle) Attach(w io.Writer) error {
	f, err := os.Open(h.LogFile.Name())
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(w, f)
	return err
}
