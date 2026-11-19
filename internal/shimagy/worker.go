// Package shimagy hosts AGY's supported per-turn subprocess convention behind
// an internal streaming input channel. The worker and its children belong to
// the existing shim's sandbox, resource limits and process group.
package shimagy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
	gop "github.com/hollis-labs/libs/harness/core/adapters/provider"
	"github.com/hollis-labs/libs/harness/core/agentlaunch"
	"github.com/hollis-labs/libs/llm/core/contracts/runtimes"
)

const MaxConfig = 64 << 10
const maxLine = 4 << 20

// Config carries the already resolved provider launch, without credentials or
// authority. Every actual turn is resolved through the published template.
type Config struct {
	Binary       string                   `json:"binary"`
	Launch       agentlaunch.TurnTemplate `json:"launch"`
	ExtraArgs    []string                 `json:"extra_args,omitempty"`
	SystemPrompt string                   `json:"system_prompt,omitempty"`
	ResumeID     string                   `json:"resume_id,omitempty"`
}

func (c Config) Validate() error {
	if !filepath.IsAbs(c.Binary) || c.Launch.Convention.Mode != runtimes.ModeSubprocessPerTurn {
		return errors.New("AGY worker requires a resolved subprocess launch")
	}
	_, err := c.Launch.TurnArgv(gop.TurnInput{SystemPrompt: c.SystemPrompt, ResumeID: c.ResumeID}, c.ExtraArgs...)
	return err
}

// Run serializes framed turns while reading internal interrupt controls. EOF
// drains accepted turns; bridge detachment never cancels the active child.
func Run(ctx context.Context, c Config, input io.Reader, output, stderr io.Writer) error {
	if err := c.Validate(); err != nil {
		return err
	}
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	output = &lockedWriter{w: output}
	type frame struct {
		Type      string `json:"type"`
		RequestID string `json:"request_id"`
		Request   struct {
			Subtype string `json:"subtype"`
		} `json:"request"`
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
	}
	frames := make(chan frame)
	scanErr := make(chan error, 1)
	go func() {
		defer close(frames)
		scanner := bufio.NewScanner(input)
		scanner.Buffer(make([]byte, 4096), maxLine)
		for scanner.Scan() {
			var f frame
			if err := json.Unmarshal(scanner.Bytes(), &f); err != nil {
				scanErr <- err
				return
			}
			select {
			case frames <- f:
			case <-ctx.Done():
				scanErr <- ctx.Err()
				return
			}
		}
		scanErr <- scanner.Err()
	}()
	type completion struct {
		native string
		err    error
	}
	var done chan completion
	var cancel context.CancelFunc
	defer func() {
		if cancel != nil {
			cancel()
			<-done
		}
	}()
	native := c.ResumeID
	var queue []string
	for {
		if done == nil && len(queue) > 0 {
			prompt := queue[0]
			queue = queue[1:]
			turnCtx, turnCancel := context.WithCancel(ctx)
			cancel = turnCancel
			done = make(chan completion, 1)
			result := done
			previous := native
			go func() {
				id := uuid.NewString()
				next, err := runTurn(turnCtx, c, prompt, previous, id, output, stderr)
				if err != nil {
					failure := map[string]any{"event": "result", "uuid": id, "result": map[string]string{"status": "ERROR", "error": "hosted AGY turn failed"}}
					if writeErr := json.NewEncoder(output).Encode(failure); writeErr != nil {
						err = writeErr
					}
				}
				result <- completion{next, err}
			}()
		}
		if frames == nil && done == nil && len(queue) == 0 {
			return <-scanErr
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case f, ok := <-frames:
			if !ok {
				frames = nil
				continue
			}
			if f.Type == "control_request" && f.Request.Subtype == "interrupt" && f.RequestID != "" {
				if cancel != nil {
					cancel()
				}
				if err := json.NewEncoder(output).Encode(map[string]any{"type": "control_response", "response": map[string]string{"subtype": "success", "request_id": f.RequestID}}); err != nil {
					return err
				}
				continue
			}
			if f.Type != "user" || f.Message.Role != "user" {
				return errors.New("AGY worker refused an invalid user frame")
			}
			if len(queue) >= 256 {
				return errors.New("AGY worker input queue full")
			}
			queue = append(queue, f.Message.Content)
		case result := <-done:
			cancel()
			cancel = nil
			done = nil
			if result.err != nil {
				return result.err
			}
			native = result.native
		}
	}
}

type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.w.Write(p)
}

func runTurn(ctx context.Context, c Config, prompt, native, id string, output, stderr io.Writer) (string, error) {
	args, err := c.Launch.TurnArgv(gop.TurnInput{Prompt: prompt, SystemPrompt: c.SystemPrompt, ResumeID: native}, c.ExtraArgs...)
	if err != nil {
		return "", err
	}
	// No new process group: the shim's authenticated stop policy owns the
	// worker and every per-turn child together. AGY stdin remains at EOF.
	cmd := exec.CommandContext(ctx, c.Binary, args...) //nolint:gosec // Catalog-resolved binary and typed resolved launch; no shell.
	cmd.WaitDelay = time.Second
	tail := &stderrTail{writer: stderr}
	cmd.Stderr = tail
	stdout, outputPipe, err := os.Pipe()
	if err != nil {
		return "", err
	}
	defer func() { _ = stdout.Close() }()
	cmd.Stdout = outputPipe
	if err = cmd.Start(); err != nil {
		_ = outputPipe.Close()
		return "", err
	}
	_ = outputPipe.Close()
	readerDone := make(chan struct{})
	processDone := make(chan error, 1)
	go func() {
		waitErr := cmd.Wait()
		// A descendant retaining stdout cannot hide the actual turn child's
		// exit indefinitely. Unknown trailing output refuses completion.
		timer := time.NewTimer(time.Second)
		defer timer.Stop()
		select {
		case <-readerDone:
		case <-timer.C:
			_ = stdout.Close()
		}
		processDone <- waitErr
	}()
	lines := bufio.NewScanner(stdout)
	lines.Buffer(make([]byte, 4096), maxLine)
	observed := ""
	var terminal map[string]json.RawMessage
	var readErr error
	for lines.Scan() {
		var raw map[string]json.RawMessage
		if json.Unmarshal(lines.Bytes(), &raw) != nil {
			continue // AGY's published parser also ignores non-JSON diagnostics.
		}
		var event, conversation string
		_ = json.Unmarshal(raw["event"], &event)
		_ = json.Unmarshal(raw["conversation_id"], &conversation)
		raw["uuid"], _ = json.Marshal(id)
		if event == "init" {
			if observed != "" || conversation == "" || (native != "" && native != conversation) {
				readErr = errors.New("hosted AGY native conversation unavailable")
				break
			}
			observed = conversation
			// Existing shim checkpoint/replay recognizes this init witness;
			// the native AGY fields remain intact for its published parser.
			raw["type"] = json.RawMessage(`"system"`)
			raw["subtype"] = json.RawMessage(`"init"`)
		}
		if event == "result" {
			if terminal != nil {
				readErr = errors.New("hosted AGY duplicated terminal result")
				break
			}
			terminal = raw
			continue // Publish completion only after actual child exit is known.
		}
		if observed == "" || terminal != nil {
			readErr = errors.New("hosted AGY output has no active native conversation")
			break
		}
		if err = json.NewEncoder(output).Encode(raw); err != nil {
			readErr = err
			break
		}
	}
	if readErr == nil {
		readErr = lines.Err()
	}
	if readErr != nil {
		_ = cmd.Process.Kill()
	}
	close(readerDone)
	waitErr := <-processDone
	if ctx.Err() != nil {
		if observed == "" {
			observed = native
		}
		failure := map[string]any{"event": "result", "uuid": id, "result": map[string]string{"conversation_id": observed, "status": "ERROR", "finished_reason": "interrupted"}}
		if err := json.NewEncoder(output).Encode(failure); err != nil {
			return "", err
		}
		return observed, nil
	}
	adapter := gop.NewAntigravityAdapter()
	if readErr != nil || waitErr != nil || adapter.IsSessionLost(tail.tail) || adapter.IsNotAuthenticated(tail.tail) || observed == "" || terminal == nil {
		return "", errors.New("hosted AGY turn did not complete under its native conversation")
	}
	var result struct {
		Status string `json:"status"`
	}
	if json.Unmarshal(terminal["result"], &result) != nil || (result.Status != "SUCCESS" && result.Status != "ERROR") {
		return "", errors.New("hosted AGY result is incomplete")
	}
	terminal["uuid"], _ = json.Marshal(id)
	if err = json.NewEncoder(output).Encode(terminal); err != nil {
		return "", err
	}
	return observed, nil
}

type stderrTail struct {
	writer io.Writer
	tail   []byte
}

func (w *stderrTail) Write(p []byte) (int, error) {
	const limit = 64 << 10
	w.tail = append(w.tail, p...)
	if len(w.tail) > limit {
		w.tail = append([]byte(nil), w.tail[len(w.tail)-limit:]...)
	}
	if w.writer == nil {
		return len(p), nil
	}
	n, err := w.writer.Write(p)
	if err != nil {
		return n, fmt.Errorf("AGY stderr write: %w", err)
	}
	return n, nil
}
