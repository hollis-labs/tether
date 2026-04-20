package claudestream

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chrispian/agent-mux/internal/launch"
	"github.com/chrispian/agent-mux/internal/provider"
)

// newTestSession returns a Session configured with a scratch log path
// and a buffered Fanout for assertions.
func newTestSession(t *testing.T, plan *launch.Plan) (*Session, *bytes.Buffer) {
	t.Helper()
	dir := t.TempDir()
	buf := &bytes.Buffer{}
	sess, err := Adapter{}.Start(context.Background(), plan, provider.StartOptions{
		Workdir: dir,
		LogPath: filepath.Join(dir, "session.log"),
		Fanout:  buf,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	s, ok := sess.(*Session)
	if !ok {
		t.Fatalf("expected *Session, got %T", sess)
	}
	return s, buf
}

func TestAdapter_Static(t *testing.T) {
	a := Adapter{}
	if a.ID() != "claude-stream" {
		t.Errorf("ID = %q, want claude-stream", a.ID())
	}
	if a.Kind() != provider.RuntimeKindCLI {
		t.Errorf("Kind = %v, want CLI", a.Kind())
	}
}

func TestAdapter_PrepareRejectsEmptyCommand(t *testing.T) {
	err := Adapter{}.Prepare(context.Background(), &launch.Plan{})
	if err == nil {
		t.Fatal("expected error for empty command")
	}
}

func TestSession_StopReturnsFromWait(t *testing.T) {
	plan := &launch.Plan{Command: "sh", Args: []string{"-c", "exit 0"}}
	s, _ := newTestSession(t, plan)

	waited := make(chan struct{})
	go func() {
		_, _ = s.Wait()
		close(waited)
	}()

	// Wait should block until Stop is called.
	select {
	case <-waited:
		t.Fatal("Wait returned before Stop")
	case <-time.After(50 * time.Millisecond):
	}

	if err := s.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case <-waited:
	case <-time.After(time.Second):
		t.Fatal("Wait did not return after Stop")
	}
}

func TestSession_SendInputAfterStopErrors(t *testing.T) {
	plan := &launch.Plan{Command: "sh", Args: []string{"-c", "exit 0"}}
	s, _ := newTestSession(t, plan)
	_ = s.Stop(context.Background())
	err := s.SendInput(context.Background(), []byte("hi"))
	if err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("expected stopped error, got %v", err)
	}
}

func TestSession_SendInputEmitsEventsAndCapturesSessionID(t *testing.T) {
	// Use a shell script that mimics claude's stream-json output.
	// The adapter appends --print --output-format stream-json --verbose
	// -p <prompt> to plan.Args, so we need plan.Args to set up a
	// script that ignores those trailing args and emits fixed output.
	plan := &launch.Plan{
		Command: "sh",
		Args: []string{"-c", `cat <<'JSON'
{"type":"system","subtype":"init","session_id":"sid-abc"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hello"}]}}
{"type":"result","subtype":"success","is_error":false,"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":3,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}
JSON`, "--"},
	}
	s, buf := newTestSession(t, plan)

	if err := s.SendInput(context.Background(), []byte("hi")); err != nil {
		t.Fatalf("SendInput: %v", err)
	}
	// Wait for the turn to finish by polling s.current.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		done := s.current == nil
		s.mu.Unlock()
		if done {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if !strings.Contains(buf.String(), `"session_id":"sid-abc"`) {
		t.Errorf("fanout missing session_id event: %q", buf.String())
	}
	if !strings.Contains(buf.String(), `"text":"hello"`) {
		t.Errorf("fanout missing assistant text event: %q", buf.String())
	}
	if got := s.SessionID(); got != "sid-abc" {
		t.Errorf("captured session_id = %q, want sid-abc", got)
	}
	_ = s.Stop(context.Background())
}

func TestSession_SendInputRejectsConcurrentTurns(t *testing.T) {
	// Use sleep to keep the first turn running.
	plan := &launch.Plan{Command: "sh", Args: []string{"-c", "sleep 1; exit 0"}}
	s, _ := newTestSession(t, plan)

	if err := s.SendInput(context.Background(), []byte("hi")); err != nil {
		t.Fatalf("first SendInput: %v", err)
	}
	// Second send while first is still running should error.
	err := s.SendInput(context.Background(), []byte("hi again"))
	if !errors.Is(err, ErrTurnInFlight) {
		t.Fatalf("expected ErrTurnInFlight, got %v", err)
	}
	_ = s.Stop(context.Background())
}

func TestSession_ResizeIsNoOp(t *testing.T) {
	plan := &launch.Plan{Command: "sh", Args: []string{"-c", "exit 0"}}
	s, _ := newTestSession(t, plan)
	defer func() { _ = s.Stop(context.Background()) }()
	if err := s.Resize(context.Background(), 24, 80); err != nil {
		t.Errorf("Resize returned error: %v", err)
	}
}

func TestSession_LogFileWrittenOnTurn(t *testing.T) {
	plan := &launch.Plan{
		Command: "sh",
		Args:    []string{"-c", `echo '{"type":"rate_limit_event"}'`, "--"},
	}
	s, _ := newTestSession(t, plan)
	_ = s.SendInput(context.Background(), []byte("ignored"))
	// Wait briefly for turn to complete.
	time.Sleep(200 * time.Millisecond)
	_ = s.Stop(context.Background())

	logBytes, err := os.ReadFile(s.logFile.Name())
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !strings.Contains(string(logBytes), "rate_limit_event") {
		t.Errorf("log missing expected line: %q", logBytes)
	}
}
