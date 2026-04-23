package claudestream

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chrispian/agent-mux/internal/launch"
	"github.com/chrispian/agent-mux/internal/provider"
	"github.com/chrispian/agent-mux/internal/provider/compliance"
)

// TestCompliance runs the shared provider compliance suite against the
// claudestream adapter using a sh-backed plan so no real Claude CLI is needed.
func TestCompliance(t *testing.T) {
	_, shErr := exec.LookPath("sh")
	if shErr != nil {
		t.Skip("sh not available")
	}
	compliance.Run(t, compliance.Harness{
		NewRuntime: func(t *testing.T) provider.Runtime { return Adapter{} },
		NewPlan: func(t *testing.T) *launch.Plan {
			// Use sh as a stand-in binary so Prepare succeeds.
			return &launch.Plan{
				Command: "sh",
				Args:    []string{"-c", "exit 0"},
			}
		},
		BinarySkip: true, // skip capability tests requiring real claude binary
	})
}

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
	if !errors.Is(err, provider.ErrNoInputChannel) {
		t.Fatalf("expected provider.ErrNoInputChannel, got %v", err)
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

// waitForTurnIdle polls s.current until it's nil (turn completed) or
// the deadline elapses. Mirrors the polling pattern in
// TestSession_SendInputEmitsEventsAndCapturesSessionID.
func waitForTurnIdle(t *testing.T, s *Session, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		done := s.current == nil
		s.mu.Unlock()
		if done {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("turn did not complete within %v", within)
}

func TestSession_PresetSessionIDIsLoadedOnStart(t *testing.T) {
	dir := t.TempDir()
	plan := &launch.Plan{Command: "sh", Args: []string{"-c", "exit 0"}}
	sess, err := Adapter{}.Start(context.Background(), plan, provider.StartOptions{
		Workdir:               dir,
		LogPath:               filepath.Join(dir, "session.log"),
		ClaudeSessionIDPreset: "sid-preloaded",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	s, ok := sess.(*Session)
	if !ok {
		t.Fatalf("expected *Session, got %T", sess)
	}
	defer func() { _ = s.Stop(context.Background()) }()
	if got := s.SessionID(); got != "sid-preloaded" {
		t.Fatalf("SessionID = %q, want sid-preloaded", got)
	}
}

func TestSession_PresetCausesResumeFlagOnFirstTurn(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args.txt")
	// $0 receives argsFile (first positional); "$@" expands to the
	// adapter-added flags — --print … --resume <sid> -p <prompt>. The
	// script writes them into argsFile so the test can assert on the
	// exact args claude was invoked with, then emits a canned init
	// event on stdout so the adapter's readTurn completes cleanly.
	script := `printf "%s\n" "$@" > "$0"; printf '{"type":"system","subtype":"init","session_id":"sid-preloaded"}\n'`
	plan := &launch.Plan{
		Command: "sh",
		Args:    []string{"-c", script, argsFile},
	}
	sess, err := Adapter{}.Start(context.Background(), plan, provider.StartOptions{
		Workdir:               dir,
		LogPath:               filepath.Join(dir, "session.log"),
		ClaudeSessionIDPreset: "sid-preloaded",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	s := sess.(*Session)
	defer func() { _ = s.Stop(context.Background()) }()

	if err := s.SendInput(context.Background(), []byte("hi")); err != nil {
		t.Fatalf("SendInput: %v", err)
	}
	waitForTurnIdle(t, s, 2*time.Second)

	got, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read args file: %v", err)
	}
	if !strings.Contains(string(got), "--resume\nsid-preloaded\n") {
		t.Fatalf("expected --resume sid-preloaded in args, got:\n%s", got)
	}
}

func TestSession_FreshSessionOmitsResumeFlag(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args.txt")
	script := `printf "%s\n" "$@" > "$0"; printf '{"type":"system","subtype":"init","session_id":"sid-fresh"}\n'`
	plan := &launch.Plan{
		Command: "sh",
		Args:    []string{"-c", script, argsFile},
	}
	sess, err := Adapter{}.Start(context.Background(), plan, provider.StartOptions{
		Workdir: dir,
		LogPath: filepath.Join(dir, "session.log"),
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	s := sess.(*Session)
	defer func() { _ = s.Stop(context.Background()) }()

	if err := s.SendInput(context.Background(), []byte("hi")); err != nil {
		t.Fatalf("SendInput: %v", err)
	}
	waitForTurnIdle(t, s, 2*time.Second)

	got, _ := os.ReadFile(argsFile)
	if strings.Contains(string(got), "--resume") {
		t.Fatalf("expected no --resume on fresh session, got:\n%s", got)
	}
}

func TestSession_OnClaudeSessionIDCallbackFiresOnceForFreshSession(t *testing.T) {
	dir := t.TempDir()
	plan := &launch.Plan{
		Command: "sh",
		Args: []string{"-c", `cat <<'JSON'
{"type":"system","subtype":"init","session_id":"sid-captured"}
JSON`, "--"},
	}

	observed := make(chan string, 4)
	sess, err := Adapter{}.Start(context.Background(), plan, provider.StartOptions{
		Workdir: dir,
		LogPath: filepath.Join(dir, "session.log"),
		OnClaudeSessionID: func(sid string) {
			observed <- sid
		},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	s := sess.(*Session)
	defer func() { _ = s.Stop(context.Background()) }()

	if err := s.SendInput(context.Background(), []byte("hi")); err != nil {
		t.Fatalf("SendInput: %v", err)
	}
	waitForTurnIdle(t, s, 2*time.Second)

	select {
	case got := <-observed:
		if got != "sid-captured" {
			t.Fatalf("callback got %q, want sid-captured", got)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("callback not invoked within 500ms")
	}

	// A second init event for the SAME session_id must not refire the
	// callback — the adapter tracks "first learned" rather than "every
	// observed". Spawn a second turn with the same session_id.
	plan2 := &launch.Plan{
		Command: "sh",
		Args: []string{"-c", `cat <<'JSON'
{"type":"system","subtype":"init","session_id":"sid-captured"}
JSON`, "--"},
	}
	s.plan = plan2
	if err := s.SendInput(context.Background(), []byte("again")); err != nil {
		t.Fatalf("second SendInput: %v", err)
	}
	waitForTurnIdle(t, s, 2*time.Second)

	select {
	case got := <-observed:
		t.Fatalf("callback refired on repeat session_id: %q", got)
	case <-time.After(200 * time.Millisecond):
		// Expected — no refire.
	}
}

func TestSession_OnClaudeSessionIDCallbackSkippedWhenPresetMatches(t *testing.T) {
	dir := t.TempDir()
	plan := &launch.Plan{
		Command: "sh",
		Args: []string{"-c", `cat <<'JSON'
{"type":"system","subtype":"init","session_id":"sid-preloaded"}
JSON`, "--"},
	}

	fired := make(chan string, 1)
	sess, err := Adapter{}.Start(context.Background(), plan, provider.StartOptions{
		Workdir:               dir,
		LogPath:               filepath.Join(dir, "session.log"),
		ClaudeSessionIDPreset: "sid-preloaded",
		OnClaudeSessionID: func(sid string) {
			fired <- sid
		},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	s := sess.(*Session)
	defer func() { _ = s.Stop(context.Background()) }()

	if err := s.SendInput(context.Background(), []byte("hi")); err != nil {
		t.Fatalf("SendInput: %v", err)
	}
	waitForTurnIdle(t, s, 2*time.Second)

	select {
	case got := <-fired:
		t.Fatalf("callback unexpectedly fired with preset in place: %q", got)
	case <-time.After(200 * time.Millisecond):
		// Expected — preset already matches, no fresh info to persist.
	}
}
