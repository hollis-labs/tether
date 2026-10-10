package service

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"time"
)

type Runner func(context.Context, string, ...string) (string, error)

// RunCommand uses argv directly. Only PATH is snapshotted into the unit; no
// installing-shell hooks, credential values or caller command fragments run.
func RunCommand(ctx context.Context, command string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, command, args...) //nolint:gosec // fixed systemctl/loginctl argv from Manager
	cmd.WaitDelay = 200 * time.Millisecond
	output := &boundedOutput{}
	cmd.Stdout, cmd.Stderr = output, output
	err := cmd.Run()
	if output.overflow {
		return "", fmt.Errorf("service command output exceeded 64 KiB")
	}
	return output.buffer.String(), err
}

type boundedOutput struct {
	buffer   bytes.Buffer
	overflow bool
}

func (w *boundedOutput) Write(data []byte) (int, error) {
	size := len(data)
	left := (64 << 10) - w.buffer.Len()
	if size > left {
		w.overflow = true
		data = data[:left]
	}
	_, _ = w.buffer.Write(data)
	return size, nil
}
