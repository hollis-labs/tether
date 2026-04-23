package opencode

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
// opencode adapter. We use sh as a stand-in binary so baseline lifecycle
// tests run without requiring the real opencode binary.
func TestCompliance(t *testing.T) {
	_, shErr := exec.LookPath("sh")
	if shErr != nil {
		t.Skip("sh not available")
	}
	compliance.Run(t, compliance.Harness{
		NewRuntime: func(t *testing.T) provider.Runtime { return Adapter{} },
		NewPlan: func(t *testing.T) *launch.Plan {
			return &launch.Plan{
				Command: "sh",
				Args:    []string{"-c", "exit 0"},
			}
		},
		BinarySkip: true, // skip capability tests requiring real opencode binary
	})
}

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

func TestAdapter_Static(t *testing.T) {
	a := Adapter{}
	if a.ID() != "opencode" {
		t.Errorf("ID = %q, want opencode", a.ID())
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
		t.Fatalf("expected ErrNoInputChannel after stop, got %v", err)
	}
}

func TestSession_SendInputEmitsEventsAndCapturesSessionID(t *testing.T) {
	// Simulate opencode's --format json output. plan.Args carries the
	// subcommand ("run" in production); the adapter appends --format json
	// and the message after "--", which sh passes as positional args that
	// the script ignores.
	plan := &launch.Plan{
		Command: "sh",
		Args: []string{"-c", `cat <<'JSON'
{"type":"step_start","timestamp":1000,"sessionID":"ses_testabc","part":{"type":"step-start"}}
{"type":"text","timestamp":1001,"sessionID":"ses_testabc","part":{"type":"text","text":"hello"}}
{"type":"step_finish","timestamp":1002,"sessionID":"ses_testabc","part":{"type":"step-finish"}}
JSON`, "--"},
	}
	s, buf := newTestSession(t, plan)

	if err := s.SendInput(context.Background(), []byte("hi")); err != nil {
		t.Fatalf("SendInput: %v", err)
	}
	waitForTurnIdle(t, s, 2*time.Second)

	if !strings.Contains(buf.String(), `"sessionID":"ses_testabc"`) {
		t.Errorf("fanout missing sessionID event: %q", buf.String())
	}
	if !strings.Contains(buf.String(), `"text":"hello"`) {
		t.Errorf("fanout missing text event: %q", buf.String())
	}
	if got := s.SessionID(); got != "ses_testabc" {
		t.Errorf("captured sessionID = %q, want ses_testabc", got)
	}
	_ = s.Stop(context.Background())
}

func TestSession_SendInputRejectsConcurrentTurns(t *testing.T) {
	plan := &launch.Plan{Command: "sh", Args: []string{"-c", "sleep 1; exit 0"}}
	s, _ := newTestSession(t, plan)

	if err := s.SendInput(context.Background(), []byte("hi")); err != nil {
		t.Fatalf("first SendInput: %v", err)
	}
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
		Args:    []string{"-c", `echo '{"type":"text","sessionID":"ses_x","part":{"text":"hi"}}'`, "--"},
	}
	s, _ := newTestSession(t, plan)
	_ = s.SendInput(context.Background(), []byte("ignored"))
	time.Sleep(200 * time.Millisecond)
	_ = s.Stop(context.Background())

	logBytes, err := os.ReadFile(s.logFile.Name())
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !strings.Contains(string(logBytes), `"text":"hi"`) {
		t.Errorf("log missing expected line: %q", logBytes)
	}
}

func TestSession_PresetSessionIDIsLoadedOnStart(t *testing.T) {
	dir := t.TempDir()
	plan := &launch.Plan{Command: "sh", Args: []string{"-c", "exit 0"}}
	sess, err := Adapter{}.Start(context.Background(), plan, provider.StartOptions{
		Workdir:               dir,
		LogPath:               filepath.Join(dir, "session.log"),
		ClaudeSessionIDPreset: "ses_preloaded",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	s, ok := sess.(*Session)
	if !ok {
		t.Fatalf("expected *Session, got %T", sess)
	}
	defer func() { _ = s.Stop(context.Background()) }()
	if got := s.SessionID(); got != "ses_preloaded" {
		t.Fatalf("SessionID = %q, want ses_preloaded", got)
	}
}

func TestSession_PresetCausesSessionFlagOnFirstTurn(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args.txt")
	// Script writes all positional args to argsFile, then emits a canned
	// JSON event so readTurn completes cleanly.
	script := `printf "%s\n" "$@" > "$0"; printf '{"type":"step_finish","sessionID":"ses_preloaded","part":{}}\n'`
	plan := &launch.Plan{
		Command: "sh",
		Args:    []string{"-c", script, argsFile},
	}
	sess, err := Adapter{}.Start(context.Background(), plan, provider.StartOptions{
		Workdir:               dir,
		LogPath:               filepath.Join(dir, "session.log"),
		ClaudeSessionIDPreset: "ses_preloaded",
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
	// Adapter builds: run --format json --session ses_preloaded <msg>
	if !strings.Contains(string(got), "--session\nses_preloaded\n") {
		t.Fatalf("expected --session ses_preloaded in args, got:\n%s", got)
	}
}

func TestSession_FreshSessionOmitsSessionFlag(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args.txt")
	script := `printf "%s\n" "$@" > "$0"; printf '{"type":"step_finish","sessionID":"ses_new","part":{}}\n'`
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
	if strings.Contains(string(got), "--session") {
		t.Fatalf("expected no --session on fresh session, got:\n%s", got)
	}
}

func TestSession_BootPromptPrependedOnFirstTurn(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args.txt")
	// Last positional arg is the user message (with boot prompt prepended).
	script := `printf "%s\n" "$@" > "$0"; printf '{"type":"step_finish","sessionID":"ses_bp","part":{}}\n'`
	plan := &launch.Plan{
		Command: "sh",
		Args:    []string{"-c", script, argsFile},
	}
	sess, err := Adapter{}.Start(context.Background(), plan, provider.StartOptions{
		Workdir:    dir,
		LogPath:    filepath.Join(dir, "session.log"),
		BootPrompt: "## Context\nboot context here",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	s := sess.(*Session)
	defer func() { _ = s.Stop(context.Background()) }()

	if err := s.SendInput(context.Background(), []byte("user msg")); err != nil {
		t.Fatalf("SendInput: %v", err)
	}
	waitForTurnIdle(t, s, 2*time.Second)

	got, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read args file: %v", err)
	}
	content := string(got)
	if !strings.Contains(content, "boot context here") {
		t.Fatalf("expected boot prompt in first-turn message, got:\n%s", content)
	}
	if !strings.Contains(content, "user msg") {
		t.Fatalf("expected user msg in first-turn message, got:\n%s", content)
	}
}

func TestSession_BootPromptSkippedOnResumedSession(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args.txt")
	script := `printf "%s\n" "$@" > "$0"; printf '{"type":"step_finish","sessionID":"ses_resume","part":{}}\n'`
	plan := &launch.Plan{
		Command: "sh",
		Args:    []string{"-c", script, argsFile},
	}
	sess, err := Adapter{}.Start(context.Background(), plan, provider.StartOptions{
		Workdir:               dir,
		LogPath:               filepath.Join(dir, "session.log"),
		BootPrompt:            "boot context here",
		ClaudeSessionIDPreset: "ses_resume",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	s := sess.(*Session)
	defer func() { _ = s.Stop(context.Background()) }()

	if err := s.SendInput(context.Background(), []byte("user msg")); err != nil {
		t.Fatalf("SendInput: %v", err)
	}
	waitForTurnIdle(t, s, 2*time.Second)

	got, _ := os.ReadFile(argsFile)
	if strings.Contains(string(got), "boot context") {
		t.Fatalf("boot prompt must not be re-injected on resumed session, got:\n%s", got)
	}
}

func TestSession_OnClaudeSessionIDCallbackFiresOnce(t *testing.T) {
	dir := t.TempDir()
	plan := &launch.Plan{
		Command: "sh",
		Args: []string{"-c", `cat <<'JSON'
{"type":"step_start","sessionID":"ses_captured","part":{}}
{"type":"step_finish","sessionID":"ses_captured","part":{}}
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
		if got != "ses_captured" {
			t.Fatalf("callback got %q, want ses_captured", got)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("callback not invoked within 500ms")
	}

	// Subsequent events with the same session ID must not refire.
	select {
	case got := <-observed:
		t.Fatalf("callback refired on repeat sessionID: %q", got)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestSession_OnClaudeSessionIDSkippedWhenPresetMatches(t *testing.T) {
	dir := t.TempDir()
	plan := &launch.Plan{
		Command: "sh",
		Args:    []string{"-c", `printf '{"type":"step_finish","sessionID":"ses_preloaded","part":{}}\n'`, "--"},
	}

	fired := make(chan string, 1)
	sess, err := Adapter{}.Start(context.Background(), plan, provider.StartOptions{
		Workdir:               dir,
		LogPath:               filepath.Join(dir, "session.log"),
		ClaudeSessionIDPreset: "ses_preloaded",
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
	}
}
