package workerruntime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/workerwire"
)

func TestWorkerCodexProtocolFixture(t *testing.T) {
	if os.Getenv("TETHER_WORKER_PROTOCOL_FIXTURE") != "1" {
		return
	}
	if slices.Contains(os.Args, "exec") {
		if !slices.Contains(os.Args, "worker prompt") {
			os.Exit(26)
		}
		for _, frame := range []any{
			map[string]any{"type": "thread.started", "thread_id": "thread-fixture"},
			map[string]any{"type": "item.completed", "item": map[string]string{"type": "agent_message", "text": "worker answer"}},
			map[string]string{"type": "turn.completed"},
		} {
			_ = json.NewEncoder(os.Stdout).Encode(frame)
		}
		os.Exit(0)
	}
	if slices.Contains(os.Args, "-p=worker prompt") {
		for _, frame := range []any{
			map[string]string{"event": "init", "conversation_id": "agy-fixture"},
			map[string]any{"event": "result", "result": map[string]string{"status": "SUCCESS", "response": "worker answer"}},
		} {
			_ = json.NewEncoder(os.Stdout).Encode(frame)
		}
		os.Exit(0)
	}
	if !slices.Contains(os.Args, "app-server") {
		os.Exit(21)
	}
	initialized, notified, thread := false, false, false
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var f struct {
			ID     any             `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(scanner.Bytes(), &f) != nil {
			os.Exit(22)
		}
		var result any = map[string]any{}
		switch f.Method {
		case "initialize":
			initialized = true
		case "initialized":
			notified = true
			continue
		case "thread/start":
			if !initialized || !notified {
				os.Exit(23)
			}
			thread = true
			result = map[string]any{"thread": map[string]string{"id": "thread-fixture"}}
		case "turn/start":
			if !thread || !bytes.Contains(f.Params, []byte("worker prompt")) {
				os.Exit(24)
			}
			result = map[string]any{"turn": map[string]string{"id": "turn-fixture"}}
		default:
			os.Exit(25)
		}
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"id": f.ID, "result": result})
		if f.Method == "turn/start" {
			_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"method": "item/completed", "params": map[string]any{"item": map[string]string{"type": "agentMessage", "text": "worker answer"}}})
			_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "thread-fixture", "turn": map[string]string{"id": "turn-fixture", "status": "completed"}}})
		}
	}
	os.Exit(0)
}

func TestWorkerCodexPreparedLaunchCompletesProtocolTurn(t *testing.T) {
	for _, tc := range []struct{ provider, mode string }{{"codex", "jsonrpc-stdio"}, {"codex", "subprocess"}, {"antigravity", "subprocess"}} {
		t.Run(tc.provider+"/"+tc.mode, func(t *testing.T) { testPreparedProviderTurn(t, tc.provider, tc.mode) })
	}
}

func testPreparedProviderTurn(t *testing.T, brand, mode string) {
	t.Setenv("HOME", t.TempDir())
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TETHER_WORKER_PROTOCOL_FIXTURE", "1")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "custom-codex")
	quoted := "'" + strings.ReplaceAll(exe, "'", "'\"'\"'") + "'"
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexec "+quoted+" -test.run=^TestWorkerCodexProtocolFixture$ -- \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	root, cwd := t.TempDir(), t.TempDir()
	e := NewExecutor(t.TempDir(), "/opt/tether-worker", nil)
	e.HubMCPURL = "https://hub.example/mcp"
	e.Tokens = &TokenMinter{}
	spec := workerwire.LaunchSpec{Session: "protocol-fixture", Provider: brand, RuntimeKind: mode, PermissionMode: "bypass", MCPPolicy: true,
		AgentTokens: []workerwire.AgentTokenSpec{{Aud: "tether", Scopes: []string{"tether.read"}}}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p, err := e.prepareProvider(ctx, spec, binary, cwd, root)
	if err != nil {
		t.Fatal(err)
	}
	args, err := providerProcessArgs(root, spec, p)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err = RunProviderProcess(ctx, args[2], strings.NewReader("{\"type\":\"user\",\"message\":{\"role\":\"user\",\"content\":\"worker prompt\"}}\n"), &out, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "worker answer") {
		t.Fatalf("no completed provider turn: %s", out.String())
	}
}

func TestWorkerTurnCompletionUsesProviderTerminal(t *testing.T) {
	for _, p := range []string{"claude", "codex", "antigravity"} {
		if workerTurnComplete(p, []byte(`{"type":"system","method":"thread/started","event":"init"}`)) {
			t.Fatal("initialization ended a turn", p)
		}
	}
	for p, frame := range map[string]string{"claude": `{"type":"result"}`, "codex": `{"method":"turn/completed"}`, "antigravity": `{"event":"result"}`} {
		if !workerTurnComplete(p, []byte(frame)) {
			t.Fatalf("%s terminal ignored", p)
		}
	}
}
