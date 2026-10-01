package acp

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/agent-contracts-leaf/runtimes"
	"github.com/hollis-labs/agentkit/agentsessions"
	"github.com/hollis-labs/go-providers/providertest"
	"github.com/hollis-labs/go-runtime-events/runtimeevents"
)

// lockedBuffer is a Fanout stand-in the test can read while the session
// writes to it.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// Copilot and Pi launch through go-agent-wrapper's launch.Select against
// go-providers' fake CLIs replaying captured ACP turns: the session is
// established, a turn's reply reaches session.log and the attach fan-out,
// and Stop ends the run.
func TestACPRuntime_TurnThroughWrapper(t *testing.T) {
	for _, tc := range []struct {
		runtime runtimes.ID
		run     providertest.Run
	}{
		{runtimes.Copilot, providertest.Replay("copilot/acp_turn").When("--acp")},
		{runtimes.Pi, providertest.Replay("pi/acp_turn")},
	} {
		t.Run(string(tc.runtime), func(t *testing.T) {
			fake := providertest.New(t, tc.runtime, tc.run)
			rt, err := New(string(tc.runtime), string(tc.runtime), runtimes.ModeACPStdio, fake.Path)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if rt.Kind() != Kind {
				t.Fatalf("Kind = %q, want %q", rt.Kind(), Kind)
			}
			if err := rt.Prepare(context.Background()); err != nil {
				t.Fatalf("Prepare: %v", err)
			}
			ws := t.TempDir()
			logPath := filepath.Join(ws, "logs", "session.log")
			fanout := &lockedBuffer{}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			sess, err := rt.Start(ctx, agentsessions.StartOptions{
				Workdir:      t.TempDir(),
				WorkspaceDir: ws,
				LogPath:      logPath,
				Fanout:       fanout,
			})
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			if h := sess.Health(); !h.Alive {
				t.Fatalf("Health after Start = %+v, want alive", h)
			}
			// ACP prompts are accepted, not awaited: the turn ends with
			// its own event.
			if err := sess.SendInput(ctx, []byte("say hi")); err != nil {
				t.Fatalf("SendInput: %v", err)
			}
			waitFor(t, fanout, "[turn_done]")
			if err := sess.Stop(ctx); err != nil {
				t.Fatalf("Stop: %v", err)
			}
			if code, err := sess.Wait(); err != nil || code != 0 {
				t.Fatalf("Wait = %d, %v; want 0 after Stop", code, err)
			}
			if h := sess.Health(); h.Alive {
				t.Fatalf("Health after Stop = %+v, want not alive", h)
			}
			if err := sess.SendInput(ctx, []byte("again")); !errors.Is(err, agentsessions.ErrNoInputChannel) {
				t.Fatalf("SendInput after Stop = %v, want ErrNoInputChannel", err)
			}

			log, err := os.ReadFile(logPath) //nolint:gosec // test-owned path
			if err != nil {
				t.Fatalf("read session.log: %v", err)
			}
			for _, want := range []string{"Hi!", "[turn_done]"} {
				if !strings.Contains(string(log), want) {
					t.Errorf("session.log missing %q:\n%s", want, log)
				}
				if !strings.Contains(fanout.String(), want) {
					t.Errorf("attach fan-out missing %q:\n%s", want, fanout.String())
				}
			}
		})
	}
}

func waitFor(t *testing.T, b *lockedBuffer, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(b.String(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %q in output:\n%s", want, b.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestNew_RejectsNonACPModeAndUnknownRuntime(t *testing.T) {
	if _, err := New("copilot", "copilot", runtimes.ModeSubprocessPerTurn, ""); err == nil {
		t.Error("New with a native mode succeeded, want an error")
	}
	if _, err := New("x", "no-such-runtime", runtimes.ModeACPStdio, ""); err == nil {
		t.Error("New with an unknown runtime succeeded, want an error")
	}
}

func TestPrepare_MissingBinary(t *testing.T) {
	rt, err := New("pi", "pi", runtimes.ModeACPStdio, filepath.Join(t.TempDir(), "no-pi-here"))
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.Prepare(context.Background()); err == nil {
		t.Fatal("Prepare with a missing binary succeeded, want an error")
	}
}

// A run that ends in an error leaves its reason in session.log; Wait alone
// reports only the exit code.
func TestFinish_RecordsTheFailureReason(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "logs", "session.log")
	out, err := openOutput(agentsessions.StartOptions{LogPath: logPath})
	if err != nil {
		t.Fatal(err)
	}
	s := &session{out: out, done: make(chan struct{})}
	s.finish(errors.New("wrapper: ACP session ended: agent exited 3"))
	if code, _ := s.Wait(); code != 1 {
		t.Fatalf("Wait code = %d, want 1", code)
	}
	data, err := os.ReadFile(logPath) //nolint:gosec // test-owned path
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "[error] wrapper: ACP session ended: agent exited 3") {
		t.Fatalf("session.log has no failure reason:\n%s", data)
	}
}

func TestRender_DeliberateKinds(t *testing.T) {
	ev := func(kind runtimeevents.EventKind, payload string) runtimeevents.Event {
		return runtimeevents.Event{Kind: kind, Payload: []byte(payload)}
	}
	for _, tc := range []struct {
		ev   runtimeevents.Event
		want string
	}{
		{ev(runtimeevents.KindAgentDelta, `{"content":"hello","phase":"message"}`), "hello"},
		{ev(runtimeevents.KindAgentDelta, `{"content":"musing","phase":"thought"}`), ""},
		{ev(runtimeevents.KindAgentDelta, `{"content":"musing","thinking":true}`), ""},
		{ev(runtimeevents.KindAgentToolUse, `{"tool_use":{"name":"read_file"}}`), "\n[tool_use:read_file]\n"},
		{ev(runtimeevents.KindTurnCompleted, `{}`), "\n[turn_done]\n"},
		{ev(runtimeevents.KindTurnFailed, `{"error":"boom"}`), "\n[error] boom\n"},
		{ev(runtimeevents.KindAgentPermissionDenied, `{"display_name":"Bash"}`), "\n[permission_denied:Bash]\n"},
		{ev(runtimeevents.KindSessionAuthFailed, `{"error":"x"}`), "\n[auth_failed]\n"},
		{ev(runtimeevents.KindSessionLost, `{}`), "\n[session_lost]\n"},
		{ev(runtimeevents.KindSessionHeartbeat, `{}`), ""},
		{ev(runtimeevents.KindStdinWrite, `{"bytes":"x"}`), ""},
	} {
		if got := render(tc.ev); got != tc.want {
			t.Errorf("render(%s %s) = %q, want %q", tc.ev.Kind, tc.ev.Payload, got, tc.want)
		}
	}
}
