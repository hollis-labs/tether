package workerruntime

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"github.com/google/uuid"
	"io"
	"os/exec"
	"time"

	gop "github.com/hollis-labs/libs/harness/core/adapters/provider"
)

// Each exec turn uses the native thread returned by the previous turn. The
// shim owns this bridge and all children in the same process group.
func runCodexExec(ctx context.Context, cfg ProviderProcess, input io.Reader, output, stderr io.Writer) error {
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 4096), 4<<20)
	native := ""
	for scanner.Scan() {
		var frame struct {
			Type    string `json:"type"`
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(scanner.Bytes(), &frame) != nil || frame.Type != "user" || frame.Message.Role != "user" {
			return fmt.Errorf("invalid provider user frame")
		}
		args, err := cfg.Launch.TurnArgv(gop.TurnInput{Prompt: frame.Message.Content, ResumeID: native}, cfg.ExtraArgs...)
		if err != nil {
			return err
		}
		cmd := exec.CommandContext(ctx, cfg.Binary, args...) //nolint:gosec // G204: local provider pin and shared native launch projection, no shell.
		cmd.Dir, cmd.Stderr = cfg.Workdir, stderr
		cmd.WaitDelay = time.Second
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return err
		}
		if err := cmd.Start(); err != nil {
			return err
		}
		lines := bufio.NewScanner(stdout)
		lines.Buffer(make([]byte, 4096), 4<<20)
		var terminal []byte
		var observed string
		var finalText string
		failed := false
		var readErr error
		for lines.Scan() {
			var event struct {
				Type     string `json:"type"`
				ThreadID string `json:"thread_id"`
				Item     struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"item"`
			}
			if json.Unmarshal(lines.Bytes(), &event) != nil {
				continue
			}
			if event.Type == "thread.started" {
				observed = event.ThreadID
				if observed == "" || (native != "" && native != observed) {
					readErr = fmt.Errorf("codex changed its native thread")
					break
				}
			}
			if event.Type == "turn.completed" || event.Type == "turn.failed" {
				failed = event.Type == "turn.failed"
				terminal = append([]byte(nil), lines.Bytes()...)
				continue
			}
			if event.Type == "item.completed" && event.Item.Type == "agent_message" {
				finalText = event.Item.Text
			}
			if _, err := fmt.Fprintln(output, string(lines.Bytes())); err != nil {
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
		waitErr := cmd.Wait()
		if readErr != nil {
			return readErr
		}
		if waitErr != nil {
			return fmt.Errorf("codex turn failed: %w", waitErr)
		}
		if observed == "" || len(terminal) == 0 {
			return fmt.Errorf("codex turn ended without native completion")
		}
		if _, err := fmt.Fprintln(output, string(terminal)); err != nil {
			return err
		}
		if err := writeProviderResult(output, "codex", uuid.NewString(), finalText, failed); err != nil {
			return err
		}
		native = observed
	}
	return scanner.Err()
}
