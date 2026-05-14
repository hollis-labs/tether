package app

import (
	"encoding/json"
	"os/exec"
	"testing"

	"github.com/hollis-labs/go-agent-sessions/agentsessions"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launch"
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
	factory, err := runtimeFactoryForProvider(config.Provider{
		ID:          "claude-code",
		Provider:    "claude",
		RuntimeKind: config.RuntimeKindStreamingStdio,
	})
	if err != nil {
		t.Fatalf("runtimeFactoryForProvider: %v", err)
	}
	rt, err := factory(&launch.Plan{Command: shPath, Args: []string{"-c", "cat"}})
	if err != nil {
		t.Fatalf("factory: %v", err)
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
	factory, err := runtimeFactoryForProvider(config.Provider{
		ID:          "codex-app-server",
		Provider:    "codex",
		RuntimeKind: config.RuntimeKindJSONRPCStdio,
	})
	if err != nil {
		t.Fatalf("runtimeFactoryForProvider: %v", err)
	}
	rt, err := factory(&launch.Plan{Command: shPath, Args: []string{"-c", "cat"}})
	if err != nil {
		t.Fatalf("factory: %v", err)
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

func TestRuntimeFactoryForProvider_ClaudePTY(t *testing.T) {
	shPath, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not available")
	}
	factory, err := runtimeFactoryForProvider(config.Provider{
		ID:          "claude-pty",
		Provider:    "claude",
		RuntimeKind: config.RuntimeKindPTY,
	})
	if err != nil {
		t.Fatalf("runtimeFactoryForProvider: %v", err)
	}
	rt, err := factory(&launch.Plan{Command: shPath, Args: []string{"-c", "cat"}})
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	caps := rt.Caps()
	if !caps.PTY {
		t.Errorf("Caps.PTY = false, want true")
	}
	if !caps.Resize {
		t.Errorf("Caps.Resize = false, want true")
	}
	if caps.StreamingStdio || caps.JsonRpcStdio {
		t.Errorf("PTY runtime should not declare stdio lifecycle caps: %+v", caps)
	}
	if !caps.BinaryRequired {
		t.Errorf("Caps.BinaryRequired = false, want true")
	}
}

func TestRuntimeFactoryForProvider_UnsupportedCombination(t *testing.T) {
	_, err := runtimeFactoryForProvider(config.Provider{
		ID:          "opencode-jsonrpc",
		Provider:    "opencode",
		RuntimeKind: config.RuntimeKindJSONRPCStdio,
	})
	if err == nil {
		t.Fatal("expected unsupported combination error, got nil")
	}
}

func TestDeferPTYStdinBootPrompt(t *testing.T) {
	opts := agentsessions.StartOptions{
		BootPrompt: "large generated boot prompt\n",
		BootMode:   "stdin",
	}
	deferPTYStdinBootPrompt(agentsessions.Capabilities{PTY: true}, &opts)

	if opts.BootPrompt != "" {
		t.Fatalf("BootPrompt = %q, want cleared", opts.BootPrompt)
	}
	if opts.BootMode != "" {
		t.Fatalf("BootMode = %q, want cleared", opts.BootMode)
	}
	if !opts.AutoFireFirstTurn {
		t.Fatal("AutoFireFirstTurn = false, want true")
	}
	if got := string(opts.FirstTurnPayload); got != "large generated boot prompt\n" {
		t.Fatalf("FirstTurnPayload = %q", got)
	}
}

func TestDeferPTYStdinBootPrompt_NonPTYUnchanged(t *testing.T) {
	opts := agentsessions.StartOptions{
		BootPrompt: "prompt",
		BootMode:   "stdin",
	}
	deferPTYStdinBootPrompt(agentsessions.Capabilities{StreamingStdio: true}, &opts)

	if opts.BootPrompt != "prompt" || opts.BootMode != "stdin" || opts.AutoFireFirstTurn {
		t.Fatalf("non-PTY options changed unexpectedly: %+v", opts)
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
