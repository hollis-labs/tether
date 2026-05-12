package app

import (
	"encoding/json"
	"os/exec"
	"testing"

	"github.com/chrispian/agent-mux/internal/launch"
)

// TestNewClaudeCodeRuntime_UsesStreamingStdio pins the v005-05 lifecycle
// flip: the Claude runtime no longer declares PTY caps; instead it
// declares StreamingStdio (mode-5) with ProviderSessionID for --resume
// chaining.
func TestNewClaudeCodeRuntime_UsesStreamingStdio(t *testing.T) {
	shPath, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not available")
	}
	rt, err := newClaudeCodeRuntime(&launch.Plan{Command: shPath, Args: []string{"-c", "cat"}})
	if err != nil {
		t.Fatalf("newClaudeCodeRuntime: %v", err)
	}
	caps := rt.Caps()
	if caps.PTY {
		t.Errorf("Caps.PTY = true, want false (streaming-stdio path)")
	}
	if !caps.StreamingStdio {
		t.Errorf("Caps.StreamingStdio = false, want true")
	}
	if caps.JsonRpcStdio {
		t.Errorf("Caps.JsonRpcStdio = true, want false")
	}
	if !caps.ProviderSessionID {
		t.Errorf("Caps.ProviderSessionID = false, want true (--resume chaining)")
	}
}

// TestNewCodexAppServerRuntime_UsesJsonRpcStdio pins the new codex
// app-server runtime: JsonRpcStdio caps, BinaryRequired, no PTY.
func TestNewCodexAppServerRuntime_UsesJsonRpcStdio(t *testing.T) {
	shPath, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not available")
	}
	rt, err := newCodexAppServerRuntime(&launch.Plan{Command: shPath, Args: []string{"-c", "cat"}})
	if err != nil {
		t.Fatalf("newCodexAppServerRuntime: %v", err)
	}
	caps := rt.Caps()
	if !caps.JsonRpcStdio {
		t.Errorf("Caps.JsonRpcStdio = false, want true")
	}
	if caps.StreamingStdio {
		t.Errorf("Caps.StreamingStdio = true, want false")
	}
	if caps.PTY {
		t.Errorf("Caps.PTY = true, want false")
	}
	if !caps.BinaryRequired {
		t.Errorf("Caps.BinaryRequired = false, want true")
	}
}

// TestFrameUserMessage_StreamingStdio pins the NDJSON envelope shape
// for Claude's streaming-input mode. Independent of any session state
// so it exercises the pure framing logic directly.
func TestFrameUserMessage_StreamingStdio(t *testing.T) {
	out, err := frameUserMessage("hello world")
	if err != nil {
		t.Fatalf("frameUserMessage: %v", err)
	}
	if len(out) == 0 || out[len(out)-1] != '\n' {
		t.Fatalf("expected trailing newline, got %q", out)
	}
	var parsed map[string]any
	if err := json.Unmarshal(out[:len(out)-1], &parsed); err != nil {
		t.Fatalf("payload is not valid JSON: %v (raw=%q)", err, out)
	}
	if parsed["type"] != "user" {
		t.Errorf("type = %v, want %q", parsed["type"], "user")
	}
	msg, ok := parsed["message"].(map[string]any)
	if !ok {
		t.Fatalf("message is not an object: %v", parsed["message"])
	}
	if msg["role"] != "user" {
		t.Errorf("message.role = %v, want %q", msg["role"], "user")
	}
	if msg["content"] != "hello world" {
		t.Errorf("message.content = %v, want %q", msg["content"], "hello world")
	}
}
