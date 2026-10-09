//go:build smoke

// Real-binary smoke test for the Antigravity runtime. It spends model
// tokens, so it sits behind the smoke tag, and it skips when agy or its
// OAuth credentials are absent — the preflight check, so it can never fall
// into agy's browser sign-in. Run with:
//
//	go test -tags smoke -v -run TestSmoke ./internal/provider/cli/antigravity/
//
// AGY_CLI_PATH overrides the binary; otherwise agy is resolved on PATH and
// in ~/.local/bin.
package antigravity

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/substrate/harness/adapters/agentsessions"
	gop "github.com/hollis-labs/substrate/harness/adapters/provider"
	llmtypes "github.com/hollis-labs/substrate/llm-core/llmtypes"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launch"
)

func smokeRuntime(t *testing.T) agentsessions.Runtime {
	t.Helper()
	bin, ok := gop.NewAntigravityAdapter().Detect()
	if !ok {
		t.Skip("agy not found")
	}
	rt, err := New(&launch.Plan{Command: bin, PermissionMode: config.PermissionModeDefault})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := rt.Prepare(context.Background()); err != nil {
		if errors.Is(err, gop.ErrProviderNotAuthenticated) {
			t.Skipf("agy not signed in: %v", err)
		}
		t.Fatalf("Prepare: %v", err)
	}
	return rt
}

// turn sends one prompt and returns the reply text, requiring usage and a
// done event.
func turn(t *testing.T, ctx context.Context, sess agentsessions.Session, events chan llmtypes.StreamEvent, prompt string) string {
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

// TestSmokeResumeAcrossTurns: turn 2 runs in a new agy process with
// --conversation and recalls turn 1.
func TestSmokeResumeAcrossTurns(t *testing.T) {
	rt := smokeRuntime(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dir := t.TempDir()
	events := make(chan llmtypes.StreamEvent, 4096)
	sess, err := rt.Start(ctx, agentsessions.StartOptions{Workdir: dir, LogPath: filepath.Join(dir, "session.log"), EventFanout: events})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = sess.Stop(context.Background()) }()

	turn(t, ctx, sess, events, "Remember this codeword: TETHER-AGY-5. Reply only with OK.")
	ider := sess.(agentsessions.SessionIDer)
	first := ider.ProviderSessionID()
	if first == "" {
		t.Fatal("no conversation id after turn 1")
	}
	out := turn(t, ctx, sess, events, "What codeword did I ask you to remember? Reply with only the codeword.")
	if got := ider.ProviderSessionID(); got != first {
		t.Errorf("conversation id changed across turns: %q -> %q", first, got)
	}
	if !strings.Contains(out, "TETHER-AGY-5") {
		t.Errorf("turn 2 did not recall turn 1; reply:\n%s", out)
	}
}

// TestSmokeReplacedConversation: a --conversation id agy never issued runs
// the turn in a new conversation and reports the replacement.
func TestSmokeReplacedConversation(t *testing.T) {
	rt := smokeRuntime(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	const stale = "00000000-0000-0000-0000-000000000000"
	dir := t.TempDir()
	events := make(chan llmtypes.StreamEvent, 4096)
	var lost [][2]string
	sess, err := rt.Start(ctx, agentsessions.StartOptions{
		Workdir:         dir,
		LogPath:         filepath.Join(dir, "session.log"),
		SessionIDPreset: stale,
		EventFanout:     events,
		OnProviderSessionLost: func(requested, actual, _ string) {
			lost = append(lost, [2]string{requested, actual})
		},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = sess.Stop(context.Background()) }()

	turn(t, ctx, sess, events, "Reply only with OK.")
	if len(lost) != 1 || lost[0][0] != stale || lost[0][1] == "" || lost[0][1] == stale {
		t.Fatalf("OnProviderSessionLost calls = %v; want one, from the stale id to a new one", lost)
	}
}
