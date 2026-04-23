package goprovider_test

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gop "github.com/hollis-labs/go-providers/provider"

	"github.com/chrispian/agent-mux/internal/launch"
	"github.com/chrispian/agent-mux/internal/provider"
	"github.com/chrispian/agent-mux/internal/provider/cli/goprovider"
	"github.com/chrispian/agent-mux/internal/provider/compliance"
)

// TestCompliance runs the shared provider compliance suite against the
// goprovider adapter. We wire a scriptAdapter so no real external binary
// is required for baseline lifecycle tests.
func TestCompliance(t *testing.T) {
	sh, shErr := exec.LookPath("sh")
	if shErr != nil {
		t.Skip("sh not available")
	}
	// A scriptAdapter that immediately exits (no NDJSON output needed for
	// baseline lifecycle tests).
	sa := &scriptAdapter{name: "compliance-stub", script: sh}
	rt := goprovider.NewRuntime("compliance-stub", sa, sh)
	compliance.Run(t, compliance.Harness{
		NewRuntime: func(t *testing.T) provider.Runtime { return rt },
		NewPlan: func(t *testing.T) *launch.Plan {
			return &launch.Plan{}
		},
		BinarySkip: true, // skip capability tests requiring real provider binary
	})
}

// scriptAdapter is a test CLIAdapter that runs a shell script as the "binary".
// The BuildArgs call just passes the prompt as a positional argument so the
// script can use "$1" if needed.
type scriptAdapter struct {
	name   string
	script string // absolute path to shell script
}

func (a *scriptAdapter) Name() string { return a.name }
func (a *scriptAdapter) Detect() (string, bool) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		return "", false
	}
	return sh, true
}
func (a *scriptAdapter) BuildArgs(prompt, _, _ string) []string {
	return []string{a.script, prompt}
}
func (a *scriptAdapter) ParseLine(line []byte) ([]gop.StreamEvent, error) {
	return gop.NewClaudeAdapter().ParseLine(line)
}

// makeScript writes content to a temp shell script and returns its path.
func makeScript(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-cli.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+content), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	return path
}

func TestRuntime_ID(t *testing.T) {
	rt := goprovider.NewRuntime("my-cli", gop.NewClaudeAdapter(), "/usr/bin/echo")
	if rt.ID() != "my-cli" {
		t.Errorf("ID = %q, want %q", rt.ID(), "my-cli")
	}
}

func TestRuntime_Kind(t *testing.T) {
	rt := goprovider.NewRuntime("my-cli", gop.NewClaudeAdapter(), "/usr/bin/echo")
	if rt.Kind() != provider.RuntimeKindCLI {
		t.Errorf("Kind = %v, want RuntimeKindCLI", rt.Kind())
	}
}

func TestRuntime_Prepare_MissingBinary(t *testing.T) {
	rt := goprovider.NewRuntime("ghost", gop.NewClaudeAdapter(), "/nonexistent/bin/ghost")
	err := rt.Prepare(context.Background(), &launch.Plan{Command: ""})
	if err == nil {
		t.Error("expected error for missing binary, got nil")
	}
}

func TestRuntime_Prepare_BinaryExists(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not found")
	}
	rt := goprovider.NewRuntime("test", gop.NewClaudeAdapter(), sh)
	if err := rt.Prepare(context.Background(), &launch.Plan{}); err != nil {
		t.Errorf("Prepare with valid binary: %v", err)
	}
}

func TestSession_SendInput_StreamsToFanout(t *testing.T) {
	// Script outputs a session_id init event and a text delta.
	script := makeScript(t, `
echo '{"type":"system","subtype":"init","session_id":"test-sess-1","cwd":"/tmp"}'
echo '{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hello from goprovider"}]}}'
echo '{"type":"result","subtype":"success","is_error":false,"result":"hello from goprovider","stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":4}}'
`)
	sh, _ := exec.LookPath("sh")
	rt := goprovider.NewRuntime("test", &scriptAdapter{name: "test", script: script}, sh)

	pr, pw := io.Pipe()
	var buf strings.Builder
	done := make(chan struct{})
	go func() {
		defer close(done)
		io.Copy(&buf, pr)
	}()

	sess, err := rt.Start(context.Background(), &launch.Plan{}, provider.StartOptions{
		Workdir: t.TempDir(),
		LogPath: filepath.Join(t.TempDir(), "log.txt"),
		Fanout:  pw,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	if err := sess.SendInput(context.Background(), []byte("hello")); err != nil {
		t.Fatalf("SendInput: %v", err)
	}
	// SendInput is synchronous — subprocess has exited. Stop closes the session.
	sess.Stop(context.Background())
	sess.Wait()

	pw.Close()
	<-done

	output := buf.String()
	if !strings.Contains(output, "test-sess-1") {
		t.Errorf("expected session_id in fanout output; got:\n%s", output)
	}
	if !strings.Contains(output, "hello from goprovider") {
		t.Errorf("expected delta text in fanout output; got:\n%s", output)
	}
}

func TestSession_OnClaudeSessionID_Callback(t *testing.T) {
	script := makeScript(t, `
echo '{"type":"system","subtype":"init","session_id":"sess-callback-abc","cwd":"/tmp"}'
echo '{"type":"result","subtype":"success","is_error":false,"result":"done","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}'
`)
	sh, _ := exec.LookPath("sh")
	rt := goprovider.NewRuntime("test", &scriptAdapter{name: "test", script: script}, sh)

	var capturedID string
	sess, err := rt.Start(context.Background(), &launch.Plan{}, provider.StartOptions{
		Workdir: t.TempDir(),
		LogPath: filepath.Join(t.TempDir(), "log.txt"),
		Fanout:  io.Discard,
		OnClaudeSessionID: func(id string) {
			capturedID = id
		},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	if err := sess.SendInput(context.Background(), []byte("hi")); err != nil {
		t.Fatalf("SendInput: %v", err)
	}
	sess.Stop(context.Background())
	sess.Wait()

	if capturedID != "sess-callback-abc" {
		t.Errorf("OnClaudeSessionID got %q, want %q", capturedID, "sess-callback-abc")
	}
}

func TestSession_Stop_TerminatesWait(t *testing.T) {
	// Script sleeps indefinitely — Stop must interrupt it.
	script := makeScript(t, `sleep 60`)
	sh, _ := exec.LookPath("sh")
	rt := goprovider.NewRuntime("test", &scriptAdapter{name: "test", script: script}, sh)

	sess, err := rt.Start(context.Background(), &launch.Plan{}, provider.StartOptions{
		Workdir: t.TempDir(),
		LogPath: filepath.Join(t.TempDir(), "log.txt"),
		Fanout:  io.Discard,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Start a long-running turn.
	errCh := make(chan error, 1)
	go func() {
		errCh <- sess.SendInput(context.Background(), []byte("go"))
	}()

	time.Sleep(100 * time.Millisecond)
	if err := sess.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Wait should return (possibly with a non-zero exit).
	done := make(chan struct{})
	go func() {
		sess.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Error("Wait did not return after Stop within 3s")
	}
}

func TestSession_Resize_NoOp(t *testing.T) {
	sh, _ := exec.LookPath("sh")
	rt := goprovider.NewRuntime("test", gop.NewClaudeAdapter(), sh)
	sess, err := rt.Start(context.Background(), &launch.Plan{}, provider.StartOptions{
		Workdir: t.TempDir(),
		LogPath: filepath.Join(t.TempDir(), "log.txt"),
		Fanout:  io.Discard,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := sess.Resize(context.Background(), 40, 120); err != nil {
		t.Errorf("Resize should be a no-op, got: %v", err)
	}
	sess.Stop(context.Background())
	sess.Wait()
}
