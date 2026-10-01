package acp

import (
	"bytes"
	"errors"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/agent-contracts-leaf/runtimes"
	"github.com/hollis-labs/agentkit/agentsessions"
	"github.com/hollis-labs/go-providers/providertest"
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
