package agystdio_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/chatbackend"
	"github.com/hollis-labs/tether/internal/chatbackend/agystdio"
	"github.com/hollis-labs/tether/internal/chatbackend/claudestdio"
)

type executeFunc func(context.Context, string, func([]byte)) (func() error, error)

func (f executeFunc) StartTurn(ctx context.Context, text string, line func([]byte)) (func() error, error) {
	return f(ctx, text, line)
}
func TestInputContextEchoesStayPrivate(t *testing.T) {
	b := agystdio.New(agystdio.Config{Executor: executeFunc(func(_ context.Context, input string, line func([]byte)) (func() error, error) {
		return func() error {
			line([]byte(`{"event":"init","conversation_id":"echo"}`))
			echo, _ := json.Marshal(map[string]any{"event": "step_update", "step_update": map[string]any{"step_index": 0, "step_type": "user_input", "state": "DONE", "text_delta": input}})
			line(echo)
			unknown, _ := json.Marshal(map[string]any{"event": "unknown_echo", "payload": map[string]any{"input": input, "context": map[string]string{"private": "secret-context"}}})
			line(unknown)
			modelEcho, _ := json.Marshal(map[string]any{"event": "step_update", "step_update": map[string]any{"step_index": 1, "step_type": "agent_response", "state": "DONE", "text_delta": strings.ReplaceAll(input, `":"`, `": "`)}})
			line(modelEcho)
			line([]byte(`{"event":"step_update","step_update":{"step_index":2,"step_type":"agent_response","state":"DONE","text_delta":"safe answer"}}`))
			line([]byte(`{"event":"result","result":{"status":"SUCCESS","response":"safe answer"}}`))
			return nil
		}, nil
	})})
	r, err := b.Submit(context.Background(), chatbackend.Turn{Text: "public text", Context: json.RawMessage(`{"private":"secret-context"}`)})
	if err != nil {
		t.Fatal(err)
	}
	evs := collect(t, r)
	data, _ := json.Marshal(evs)
	if strings.Contains(string(data), "secret-context") || strings.Contains(string(data), "app-context") || strings.Contains(string(data), "user_input") {
		t.Fatalf("private input echo published: %s", data)
	}
	if !strings.Contains(string(data), "safe answer") {
		t.Fatalf("public output lost: %s", data)
	}
}
func TestLateHostedCancelCarriesOriginalTarget(t *testing.T) {
	for range 100 {
		tap := claudestdio.NewTap()
		ctx, cancel := context.WithCancel(context.Background())
		var target string
		e := agystdio.NewHostedExecutor(agystdio.HostedConfig{Tap: tap, Busy: func() bool { return false }, Send: func(_ context.Context, raw []byte) error {
			var frame struct {
				TurnID  string `json:"tether_turn_id"`
				Request struct {
					Target string `json:"target_turn_id"`
				} `json:"request"`
			}
			_ = json.Unmarshal(raw, &frame)
			if frame.TurnID != "" {
				target = frame.TurnID
			} else if frame.Request.Target != target || target == "" {
				t.Errorf("late cancel lost original target: %s", raw)
			}
			return nil
		}})
		wait, err := e.StartTurn(ctx, "first", func([]byte) {})
		if err != nil {
			t.Fatal(err)
		}
		cancel()
		result, _ := json.Marshal(map[string]any{"event": "result", "tether_turn_id": target, "result": map[string]string{"status": "SUCCESS"}})
		_, _ = tap.Write(append(result, '\n'))
		_, _ = tap.Write([]byte(`{"event":"init","tether_turn_id":"successor","conversation_id":"native"}` + "\n"))
		_ = wait()
		_ = e.Close()
		tap.Close()
	}
}
