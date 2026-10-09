//go:build smoke

// Real-binary smoke test for the opencode runtime. It spends model tokens,
// so it sits behind the smoke tag and skips when opencode is not installed.
// Run with:
//
//	go test -tags smoke -v -run TestSmoke ./internal/provider/cli/opencode/
//
// OPENCODE_CLI_PATH overrides the binary; otherwise opencode is resolved on
// PATH and then at ~/.opencode/bin/opencode (the installer's default).
package opencode

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/substrate/harness/adapters/agentsessions"
	gop "github.com/hollis-labs/substrate/harness/adapters/provider"
	llmtypes "github.com/hollis-labs/substrate/llm-core/llmtypes"

	"github.com/hollis-labs/tether/internal/launch"
)

func opencodeBinary(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("OPENCODE_CLI_PATH"); p != "" {
		if _, err := os.Stat(p); err != nil {
			t.Skipf("OPENCODE_CLI_PATH %q: %v", p, err)
		}
		return p
	}
	if p, err := exec.LookPath("opencode"); err == nil {
		return p
	}
	if home, err := os.UserHomeDir(); err == nil {
		p := filepath.Join(home, ".opencode", "bin", "opencode")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	t.Skip("opencode binary not found")
	return ""
}

// TestSmokeResumeAcrossTurns drives two turns through the Tether runtime and
// asserts the second turn resumed the first turn's opencode session.
func TestSmokeResumeAcrossTurns(t *testing.T) {
	bin := opencodeBinary(t)
	// Args mirrors the seeded catalog's `args: [run]`.
	rt, err := New(&launch.Plan{Command: bin, Args: []string{"run"}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	events := make(chan llmtypes.StreamEvent, 4096)
	dir := t.TempDir()
	sess, err := rt.Start(ctx, agentsessions.StartOptions{
		Workdir:     dir,
		LogPath:     filepath.Join(dir, "session.log"),
		Env:         os.Environ(),
		EventFanout: events,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = sess.Stop(context.Background()) }()

	// turn returns the reply text; typed deltas carry only the text now,
	// not raw JSON lines. It also requires usage before done.
	turn := func(prompt string) string {
		t.Helper()
		if err := sess.SendInput(ctx, []byte(prompt)); err != nil {
			t.Fatalf("SendInput(%q): %v", prompt, err)
		}
		var text strings.Builder
		var usage int
		for {
			select {
			case ev := <-events:
				switch ev.Type {
				case llmtypes.EventDelta:
					text.WriteString(ev.Content)
				case llmtypes.EventUsage:
					usage++
					if ev.Usage == nil || ev.Usage.OutputTokens == 0 {
						t.Errorf("usage event without output tokens: %+v", ev.Usage)
					}
				case llmtypes.EventError:
					t.Fatalf("turn error: %s", ev.Error)
				case llmtypes.EventDone:
					if usage == 0 {
						t.Errorf("turn %q finished with no usage event", prompt)
					}
					return text.String()
				case llmtypes.EventSessionID, llmtypes.EventToolUse, llmtypes.EventThinking:
				}
			case <-ctx.Done():
				t.Fatalf("turn timed out")
			}
		}
	}

	turn("Remember this codeword: TETHER-SMOKE-7. Reply only with OK.")
	ider, ok := sess.(agentsessions.SessionIDer)
	if !ok {
		t.Fatalf("session does not implement SessionIDer")
	}
	first := ider.ProviderSessionID()
	if !strings.HasPrefix(first, "ses_") {
		t.Fatalf("provider session id after turn 1 = %q", first)
	}

	out := turn("What codeword did I ask you to remember? Reply with only the codeword.")
	if got := ider.ProviderSessionID(); got != first {
		t.Errorf("provider session id changed across turns: %q -> %q", first, got)
	}
	if !strings.Contains(out, "TETHER-SMOKE-7") {
		t.Errorf("turn 2 did not recall turn 1; reply:\n%s", out)
	}
}

// TestSmokeLostSession resumes an id opencode never issued: the turn fails
// with ErrProviderSessionLost before any model call, and the next turn
// starts a fresh session.
func TestSmokeLostSession(t *testing.T) {
	bin := opencodeBinary(t)
	rt, err := New(&launch.Plan{Command: bin, Args: []string{"run"}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dir := t.TempDir()
	var ids []string
	sess, err := rt.Start(ctx, agentsessions.StartOptions{
		Workdir:         dir,
		LogPath:         filepath.Join(dir, "session.log"),
		Env:             os.Environ(),
		SessionIDPreset: "ses_tetherSmokeDoesNotExist",
		OnSessionID:     func(id string) { ids = append(ids, id) },
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = sess.Stop(context.Background()) }()

	err = sess.SendInput(ctx, []byte("Reply only with OK."))
	if !errors.Is(err, gop.ErrProviderSessionLost) {
		t.Fatalf("stale resume err = %v; want ErrProviderSessionLost", err)
	}
	if err := sess.SendInput(ctx, []byte("Reply only with OK.")); err != nil {
		t.Fatalf("fresh turn after a lost session: %v", err)
	}
	if len(ids) == 0 || !strings.HasPrefix(ids[0], "ses_") || ids[0] == "ses_tetherSmokeDoesNotExist" {
		t.Errorf("OnSessionID after the fresh turn = %q; want a new ses_ id", ids)
	}
}
