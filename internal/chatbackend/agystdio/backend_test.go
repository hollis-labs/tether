package agystdio_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/libs/harness/core/adapters/agentsessions"
	"github.com/hollis-labs/libs/ui/ui-go/chatstream"
	"github.com/hollis-labs/tether/internal/chatbackend"
	"github.com/hollis-labs/tether/internal/chatbackend/agystdio"
	"github.com/hollis-labs/tether/internal/chatbackend/claudestdio"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/provider/cli/antigravity"
)

const script = `#!/bin/sh
printf '%s\n' "$*" >> argv.log
conv=conversation-one
prompt=
while [ $# -gt 0 ]; do
 case "$1" in --conversation) conv=$2; shift;; -p=*) prompt=${1#-p=};; esac
 shift
done
case "$prompt" in replacement) conv=conversation-replaced;; esac
printf '{"event":"init","conversation_id":"%s"}\n' "$conv"
case "$prompt" in
 slow) exec sleep 60;;
 crash) exit 7;;
 tool) printf '%s\n' '{"event":"step_update","step_update":{"step_index":0,"step_type":"tool","state":"DONE","tool_info":{"name":"read_file","parameters":{"path":"a"},"output":"contents"}}}';;
 denied) printf '%s\n' '{"event":"result","result":{"conversation_id":"conversation-one","status":"SUCCESS","denied_actions":[{"action":"command"}]}}'; exit 0;;
 failed) printf '%s\n' '{"event":"result","result":{"conversation_id":"conversation-one","status":"ERROR","error":"failed","finished_reason":"test_failure"}}'; exit 1;;
esac
printf '%s\n' '{"event":"step_update","step_update":{"step_index":1,"step_type":"agent_response","state":"DONE","text_delta":"hello"}}'
printf '{"event":"result","result":{"conversation_id":"%s","status":"SUCCESS","response":"hello"}}\n' "$conv"
case "$prompt" in badexit) exit 9;; esac
`

type harness struct {
	b   *agystdio.Backend
	s   agentsessions.Session
	dir string
}

func start(t *testing.T, binary string, mode string) harness {
	t.Helper()
	dir := t.TempDir()
	if binary == "" {
		if runtime.GOOS == "windows" {
			t.Skip("fake agy requires sh")
		}
		binary = filepath.Join(dir, "agy")
		if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	rt, err := antigravity.New(&launch.Plan{Command: binary, PermissionMode: mode})
	if err != nil {
		t.Fatal(err)
	}
	s, err := rt.Start(context.Background(), agentsessions.StartOptions{Workdir: dir, LogPath: filepath.Join(dir, "session.log")})
	if err != nil {
		t.Fatal(err)
	}
	b := agystdio.New(agystdio.Config{Executor: rt.(agystdio.Executor)})
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { defer close(stopped); _ = b.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-stopped; _ = s.Stop(context.Background()) })
	return harness{b, s, dir}
}
func collect(t *testing.T, r chatbackend.Run) []chatstream.Event {
	t.Helper()
	done := make(chan []chatstream.Event, 1)
	go func() {
		var evs []chatstream.Event
		for ev, err := range r.Events() {
			if err != nil {
				return
			}
			evs = append(evs, ev)
		}
		done <- evs
	}()
	select {
	case evs := <-done:
		n := 0
		for _, ev := range evs {
			if ev.IsTerminal() {
				n++
			}
			if ev.RunID != r.ID() {
				t.Errorf("run id %q", ev.RunID)
			}
		}
		if len(evs) == 0 || evs[0].Verb != chatstream.VerbRunStart || n != 1 || !evs[len(evs)-1].IsTerminal() {
			t.Fatalf("invalid run: %+v", evs)
		}
		return evs
	case <-time.After(20 * time.Second):
		r.Cancel()
		t.Fatal("run timed out")
		return nil
	}
}

func TestFakeAgyCases(t *testing.T) {
	for _, tc := range []struct {
		text string
		verb chatstream.Verb
		kind string
	}{
		{"text", chatstream.VerbRunFinish, ""}, {"tool", chatstream.VerbRunFinish, "tool_call"}, {"denied", chatstream.VerbRunFinish, "agy.permission_denied"}, {"failed", chatstream.VerbRunError, ""}, {"crash", chatstream.VerbRunError, ""}, {"badexit", chatstream.VerbRunError, ""},
	} {
		t.Run(tc.text, func(t *testing.T) {
			h := start(t, "", config.PermissionModeDefault)
			r, err := h.b.Submit(context.Background(), chatbackend.Turn{Text: tc.text})
			if err != nil {
				t.Fatal(err)
			}
			evs := collect(t, r)
			if tc.text == "text" {
				var text strings.Builder
				finals := 0
				for _, ev := range evs {
					if ev.Verb == chatstream.VerbPartDelta {
						text.WriteString(ev.Text)
					}
					if ev.Verb == chatstream.VerbPartEnd && string(ev.Final) == `"hello"` {
						finals++
					}
				}
				if text.String() != "hello" || finals != 1 {
					t.Fatalf("text=%q finals=%d events=%+v", text.String(), finals, evs)
				}
			}
			if got := evs[len(evs)-1].Verb; got != tc.verb {
				t.Errorf("terminal %s want %s", got, tc.verb)
			}
			if tc.kind != "" {
				found := false
				for _, ev := range evs {
					if ev.Kind == tc.kind {
						found = true
					}
				}
				if !found {
					t.Errorf("missing %s: %+v", tc.kind, evs)
				}
			}
			if !h.b.Info().Deltas || h.b.Info().Approvals {
				t.Errorf("info %+v", h.b.Info())
			}
			select {
			case <-h.b.Observed():
				t.Fatal("unexpected observed run")
			default:
			}
		})
	}
}
func TestCancelBusyContinuityContext(t *testing.T) {
	h := start(t, "", config.PermissionModeBypass)
	r, err := h.b.Submit(context.Background(), chatbackend.Turn{Text: "slow"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.b.Submit(context.Background(), chatbackend.Turn{Text: "busy"}); !errors.Is(err, chatbackend.ErrBusy) {
		t.Fatalf("busy: %v", err)
	}
	// Ordinary mail/CLI input shares the same seat reservation.
	if err = h.s.SendInput(context.Background(), []byte("other")); !errors.Is(err, agentsessions.ErrTurnInFlight) {
		t.Fatalf("ordinary overlap: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	r.Cancel()
	r.Cancel()
	evs := collect(t, r)
	if evs[len(evs)-1].Verb != chatstream.VerbRunAbort {
		t.Fatalf("cancel: %+v", evs)
	}
	for _, text := range []string{"one", "two"} {
		r, err = h.b.Submit(context.Background(), chatbackend.Turn{Text: text, Context: json.RawMessage(`{"private":"context-only"}`)})
		if err != nil {
			t.Fatal(err)
		}
		evs = collect(t, r)
		data, _ := json.Marshal(evs)
		if strings.Contains(string(data), "context-only") || strings.Contains(string(data), "app-context") {
			t.Fatal("context leaked")
		}
	}
	args, err := os.ReadFile(filepath.Join(h.dir, "argv.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(args), "--conversation conversation-one") || !strings.Contains(string(args), "--dangerously-skip-permissions") || !strings.Contains(string(args), `-p=<app-context>{"private":"context-only"}</app-context>`) {
		t.Fatalf("argv missing continuity, posture or context: %s", args)
	}
	if id := h.s.(agentsessions.SessionIDer).ProviderSessionID(); id != "conversation-one" {
		t.Errorf("native id %q", id)
	}
}
func TestInvalidContextAndExternalBusy(t *testing.T) {
	h := start(t, "", config.PermissionModeDefault)
	if _, err := h.b.Submit(context.Background(), chatbackend.Turn{Text: "text", Context: json.RawMessage(`{`)}); err == nil {
		t.Fatal("invalid context accepted")
	}
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { done <- h.s.SendInput(ctx, []byte("slow")) }()
	deadline := time.Now().Add(5 * time.Second)
	for h.s.Health().State != agentsessions.LiveStateProcessing {
		if time.Now().After(deadline) {
			t.Fatal("turn never started")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := h.b.Submit(context.Background(), chatbackend.Turn{Text: "text"}); !errors.Is(err, chatbackend.ErrBusy) {
		t.Fatalf("external busy: %v", err)
	}
	cancel()
	<-done
}

// Explicit opt-in records one real backend turn without making ordinary tests use credentials.
func TestRealAgy(t *testing.T) {
	if os.Getenv("TETHER_TEST_REAL_AGY") != "1" {
		t.Skip("set TETHER_TEST_REAL_AGY=1 to record a real backend turn")
	}
	binary, err := exec.LookPath("agy")
	if err != nil {
		t.Skip("agy unavailable")
	}
	h := start(t, binary, config.PermissionModeDefault)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	r, err := h.b.Submit(ctx, chatbackend.Turn{Text: "Reply with exactly AGY_BACKEND_OK. Do not use tools."})
	if err != nil {
		t.Fatal(err)
	}
	evs := collect(t, r)
	var text strings.Builder
	for _, ev := range evs {
		if ev.Verb == chatstream.VerbPartDelta {
			text.WriteString(ev.Text)
			t.Logf("text: %s", ev.Text)
		}
	}
	if strings.TrimSpace(text.String()) != "AGY_BACKEND_OK" {
		t.Fatalf("unexpected real response %q", text.String())
	}
	last := evs[len(evs)-1]
	t.Logf("terminal=%s code=%s events=%d", last.Verb, last.Code, len(evs))
	if last.Verb != chatstream.VerbRunFinish {
		t.Fatalf("real backend turn failed: %s", last.Message)
	}
}

func TestReplacedNativeConversationIsNotSuccess(t *testing.T) {
	h := start(t, "", config.PermissionModeDefault)
	r, err := h.b.Submit(context.Background(), chatbackend.Turn{Text: "first"})
	if err != nil {
		t.Fatal(err)
	}
	_ = collect(t, r)
	r, err = h.b.Submit(context.Background(), chatbackend.Turn{Text: "replacement"})
	if err != nil {
		t.Fatal(err)
	}
	evs := collect(t, r)
	last := evs[len(evs)-1]
	if last.Verb != chatstream.VerbRunError || last.Code != "process_error" {
		t.Fatalf("replaced conversation reported as success: %+v", evs)
	}
}

func TestHostedPartialObservedTurnIsBusy(t *testing.T) {
	tap := claudestdio.NewTap()
	defer tap.Close()
	e := agystdio.NewHostedExecutor(agystdio.HostedConfig{Tap: tap, Busy: func() bool { return false }, Send: func(context.Context, []byte) error {
		t.Fatal("input delivered during observed partial turn")
		return nil
	}})
	defer func() { _ = e.Close() }()
	_, _ = tap.Write([]byte(`{"event":"step_update","step_update":{"step_index":1,"step_type":"agent_response","state":"ACTIVE","text_delta":"partial"}}` + "\n"))
	if _, err := e.StartTurn(context.Background(), "new", func([]byte) {}); !errors.Is(err, chatbackend.ErrBusy) {
		t.Fatalf("partial observed turn: %v", err)
	}
}
