//go:build !windows

package shimagy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gop "github.com/hollis-labs/libs/harness/core/adapters/provider"
	"github.com/hollis-labs/libs/harness/core/agentlaunch"
	"github.com/hollis-labs/libs/llm/core/contracts/runtimes"
)

func workerFixture(t *testing.T, body string) Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agy")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0700); err != nil {
		t.Fatal(err)
	}
	return Config{Binary: path, Launch: agentlaunch.TurnTemplate{Convention: gop.LaunchConvention{Executable: path, Mode: runtimes.ModeSubprocessPerTurn, Argv: []gop.ArgTemplate{{Kind: gop.ArgResume, Value: "--conversation"}, {Kind: gop.ArgPromptInline, Value: "-p=", WithSystem: true}}}}}
}

func userFrame(text string) string {
	data, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]string{"role": "user", "content": text}})
	return string(data) + "\n"
}

func workerLines(t *testing.T, output *bytes.Buffer) []map[string]json.RawMessage {
	t.Helper()
	var lines []map[string]json.RawMessage
	for _, line := range bytes.Split(bytes.TrimSpace(output.Bytes()), []byte("\n")) {
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(line, &raw); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, raw)
	}
	return lines
}

func TestWorkerKeepsNativeConversationAndUniqueTurnResults(t *testing.T) {
	cfg := workerFixture(t, `
if [ "$1" = "--conversation" ]; then
  [ "$2" = "native-1" ] || exit 4
  shift 2
else
  [ "$1" = '-p=System: system

first' ] || exit 5
fi
printf '%s\n' '{"event":"init","conversation_id":"native-1","init":{}}'
printf '%s\n' '{"event":"result","result":{"status":"SUCCESS","response":"same text"}}'
`)
	cfg.SystemPrompt = "system"
	var output, stderr bytes.Buffer
	if err := Run(context.Background(), cfg, strings.NewReader(userFrame("first")+userFrame("second")), &output, &stderr); err != nil {
		t.Fatal(err)
	}
	lines := workerLines(t, &output)
	if len(lines) != 4 || string(lines[0]["type"]) != `"system"` || string(lines[0]["subtype"]) != `"init"` {
		t.Fatalf("native init witness missing: %s", output.String())
	}
	if string(lines[1]["uuid"]) == "" || string(lines[1]["uuid"]) == string(lines[3]["uuid"]) {
		t.Fatal("separate real turns shared a result identity")
	}
	if string(lines[2]["conversation_id"]) != `"native-1"` {
		t.Fatal("native conversation changed")
	}
}

func TestWorkerRefusesIncompleteLostAndFailedTurns(t *testing.T) {
	for _, tc := range []struct {
		name, body, resume string
	}{
		{"lost conversation", `printf '%s\n' '{"event":"init","conversation_id":"replacement","init":{}}'`, "retained"},
		{"missing init", `printf '%s\n' '{"event":"result","result":{"status":"SUCCESS","response":"wrong"}}'`, ""},
		{"missing result", `printf '%s\n' '{"event":"init","conversation_id":"native","init":{}}'`, ""},
		{"nonzero after success", `printf '%s\n' '{"event":"init","conversation_id":"native","init":{}}' '{"event":"result","result":{"status":"SUCCESS","response":"wrong"}}'; exit 7`, ""},
		{"unknown status", `printf '%s\n' '{"event":"init","conversation_id":"native","init":{}}' '{"event":"result","result":{"status":"UNKNOWN"}}'`, ""},
		{"inherited pipes", `sleep 2 & printf '%s\n' '{"event":"init","conversation_id":"native","init":{}}' '{"event":"result","result":{"status":"SUCCESS"}}'`, ""},
		{"auth failure", `printf '%s\n' '{"event":"init","conversation_id":"native","init":{}}' '{"event":"result","result":{"status":"SUCCESS"}}'; printf '%s\n' 'not authenticated: no stored credentials found' >&2`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := workerFixture(t, tc.body)
			cfg.ResumeID = tc.resume
			var output, stderr bytes.Buffer
			if err := Run(context.Background(), cfg, strings.NewReader(userFrame("first")+userFrame("must not run")), &output, &stderr); err == nil {
				t.Fatal("unavailable native turn accepted")
			}
			for _, raw := range workerLines(t, &output) {
				if bytes.Contains(raw["result"], []byte("SUCCESS")) || string(raw["conversation_id"]) == `"replacement"` {
					t.Fatal("failed or replaced conversation published as success")
				}
			}
		})
	}
}

func TestWorkerInvalidInputDoesNotStartProvider(t *testing.T) {
	cfg := workerFixture(t, "exit 99")
	var output bytes.Buffer
	if err := Run(context.Background(), cfg, strings.NewReader("not a user frame\n"), &output, nil); err == nil || output.Len() != 0 {
		t.Fatal("invalid input started a turn")
	}
}

type cancelAtRead struct {
	cancel context.CancelFunc
	sent   bool
}

func (r *cancelAtRead) Read(p []byte) (int, error) {
	if r.sent {
		return 0, io.EOF
	}
	r.sent = true
	r.cancel()
	return copy(p, `{"type":"control_request","request_id":"stop","request":{"subtype":"interrupt"}}`+"\n"), nil
}
func TestWorkerCancellationAtInputBoundary(t *testing.T) {
	cfg := workerFixture(t, "exit 99")
	for range 100 {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); _ = Run(ctx, cfg, &cancelAtRead{cancel: cancel}, io.Discard, io.Discard) }()
		select {
		case <-done:
		case <-time.After(time.Second):
			cancel()
			t.Fatal("worker hung at canceled input boundary")
		}
		cancel()
	}
}

type frameSink struct {
	frames chan map[string]json.RawMessage
}

func (w frameSink) Write(p []byte) (int, error) {
	var frame map[string]json.RawMessage
	if err := json.Unmarshal(p, &frame); err != nil {
		return 0, err
	}
	w.frames <- frame
	return len(p), nil
}
func TestLateTargetedInterruptDoesNotCancelSuccessor(t *testing.T) {
	cfg := workerFixture(t, `printf '%s\n' '{"event":"init","conversation_id":"native-late"}'
case "$*" in *slow*) exec sleep 60;; esac
printf '%s\n' '{"event":"result","result":{"status":"SUCCESS"}}'
`)
	input, writer := io.Pipe()
	frames := make(chan map[string]json.RawMessage, 32)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = Run(ctx, cfg, input, frameSink{frames}, io.Discard) }()
	defer func() {
		cancel()
		_ = writer.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("worker did not stop")
		}
		_ = input.Close()
	}()
	receive := func(event string) map[string]json.RawMessage {
		t.Helper()
		for {
			select {
			case frame := <-frames:
				if string(frame["event"]) == `"result"` && event != "result" {
					t.Fatalf("successor was interrupted: %v", frame)
				}
				if string(frame["event"]) == `"`+event+`"` || (event == "control_response" && string(frame["type"]) == `"control_response"`) {
					return frame
				}
			case <-time.After(5 * time.Second):
				t.Fatal("missing worker event " + event)
				return nil
			}
		}
	}
	first := map[string]any{"type": "user", "tether_turn_id": "chat-first", "message": map[string]string{"role": "user", "content": "first"}}
	if err := json.NewEncoder(writer).Encode(first); err != nil {
		t.Fatal(err)
	}
	_ = receive("result")
	_, _ = io.WriteString(writer, userFrame("slow successor"))
	_ = receive("init")
	interrupt := `{"type":"control_request","request_id":"late","request":{"subtype":"interrupt","target_turn_id":"chat-first"}}` + "\n"
	_, _ = io.WriteString(writer, interrupt)
	_ = receive("control_response")
	select {
	case frame := <-frames:
		if string(frame["event"]) == `"result"` {
			t.Fatalf("late interrupt canceled successor: %v", frame)
		}
	case <-time.After(150 * time.Millisecond):
	}
}

func TestImmediateTargetCancelKeepsWorkerForNextTurn(t *testing.T) {
	cfg := workerFixture(t, `printf '%s\n' '{"event":"init","conversation_id":"native-cancel"}'
case "$*" in *slow*) exec sleep 60;; esac
printf '%s\n' '{"event":"result","result":{"status":"SUCCESS"}}'
`)
	for range 20 {
		var output bytes.Buffer
		first := `{"type":"user","tether_turn_id":"first","message":{"role":"user","content":"slow"}}` + "\n"
		interrupt := `{"type":"control_request","request_id":"cancel","request":{"subtype":"interrupt","target_turn_id":"first"}}` + "\n"
		if err := Run(context.Background(), cfg, strings.NewReader(first+interrupt+userFrame("next")), &output, io.Discard); err != nil {
			t.Fatalf("early cancel ended worker: %v", err)
		}
		results := 0
		for _, frame := range workerLines(t, &output) {
			if string(frame["event"]) == `"result"` {
				results++
				if results == 1 && !bytes.Contains(frame["result"], []byte("interrupted")) {
					t.Fatalf("first turn was not aborted: %s", frame["result"])
				}
				if results == 2 && !bytes.Contains(frame["result"], []byte("SUCCESS")) {
					t.Fatalf("next turn failed: %s", frame["result"])
				}
			}
		}
		if results != 2 {
			t.Fatalf("terminal count %d", results)
		}
	}
}
