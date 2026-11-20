package chatwire

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/libs/ui/ui-go/chatstream"
	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/chatbackend/agystdio"
	tstore "github.com/hollis-labs/tether/internal/store"
)

type agyEchoExecutor struct{ inputs chan string }

func (e agyEchoExecutor) StartTurn(_ context.Context, input string, line func([]byte)) (func() error, error) {
	return func() error {
		e.inputs <- input
		line([]byte(`{"event":"init","conversation_id":"echo-native"}`))
		user, _ := json.Marshal(map[string]any{"event": "step_update", "step_update": map[string]any{"step_index": 0, "step_type": "user_input", "state": "DONE", "text_delta": input}})
		line(user)
		opaque, _ := json.Marshal(map[string]any{"event": "unknown_input_echo", "input": input, "context": map[string]string{"private": "secret-app-context"}})
		line(opaque)
		model, _ := json.Marshal(map[string]any{"event": "step_update", "step_update": map[string]any{"step_index": 1, "step_type": "agent_response", "state": "DONE", "text_delta": strings.ReplaceAll(input, `":"`, `": "`)}})
		line(model)
		line([]byte(`{"event":"step_update","step_update":{"step_index":2,"step_type":"agent_response","state":"DONE","text_delta":"safe answer"}}`))
		line([]byte(`{"event":"result","result":{"conversation_id":"echo-native","status":"SUCCESS","response":"safe answer"}}`))
		return nil
	}, nil
}

type agyEchoSessions struct{ backend Backend }

func (s agyEchoSessions) ChatRuntime(string) (Runtime, error)          { return RuntimeAgyStdio, nil }
func (s agyEchoSessions) ChatBackend(string, Runtime) (Backend, error) { return s.backend, nil }

// Exercise the real HTTP wire, runner, agy decoder and SQLite event log. Input
// echoes must not publish private context in either live SSE or durable history.
func TestAgyContextEchoStaysOutOfPersistedEventLog(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := tstore.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	(&env{st: st}).session(t, "agy-session", "agy-seat", "")
	inputs := make(chan string, 1)
	backend := agystdio.New(agystdio.Config{Executor: agyEchoExecutor{inputs: inputs}})
	w, err := Open(ctx, Config{StateDir: dir, Store: st, Sessions: agyEchoSessions{backend: backend}})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(api.NewHandler(api.Deps{ChatWire: w.Handler()}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { server.CloseClientConnections(); _ = w.Close(context.Background()) })
	c := client{t: t, base: server.URL}
	status, conversation := c.do(http.MethodPost, "/chat/v1/agents/agy-seat/conversation", nil)
	if status != http.StatusOK {
		t.Fatalf("conversation: %d %v", status, conversation)
	}
	id := conversation["conversation_id"].(string)
	status, attachment := c.do(http.MethodPost, "/chat/v1/conversations/"+id+"/attachments", map[string]string{"role": "controller", "stream": "deltas"})
	if status != http.StatusCreated {
		t.Fatalf("attachment: %d %v", status, attachment)
	}
	controller := attachment["attachment_id"].(string)
	stream, status := c.stream(id, controller, "0")
	if status != http.StatusOK {
		t.Fatalf("stream: %d", status)
	}
	status, receipt := c.do(http.MethodPost, "/chat/v1/conversations/"+id+"/turns", map[string]any{"attachment": controller, "text": "public prompt", "request_id": "agy-private", "context": json.RawMessage(`{"private":"secret-app-context"}`)})
	if status != http.StatusAccepted {
		t.Fatalf("submit: %d %v", status, receipt)
	}
	events := until(t, stream, func(ev chatstream.Event) bool { return ev.IsTerminal() })
	if events[len(events)-1].Ev.Verb != chatstream.VerbRunFinish {
		t.Fatalf("run failed: %+v", events)
	}
	for _, ev := range events {
		if strings.Contains(ev.Raw, "secret-app-context") || strings.Contains(ev.Raw, "app-context") {
			t.Fatalf("private context reached SSE: %s", ev.Raw)
		}
	}
	select {
	case input := <-inputs:
		if !strings.Contains(input, `<app-context>{"private":"secret-app-context"}</app-context>`) {
			t.Fatalf("context not delivered to agent: %q", input)
		}
	case <-time.After(time.Second):
		t.Fatal("agent received no input")
	}
	count, terminals := 0, 0
	public, answer := false, false
	for record, err := range w.db.Log.After(ctx, id, 0) {
		if err != nil {
			t.Fatal(err)
		}
		count++
		if bytes.Contains(record.Data, []byte("secret-app-context")) || bytes.Contains(record.Data, []byte("app-context")) {
			t.Fatalf("private context persisted: %s", record.Data)
		}
		public = public || bytes.Contains(record.Data, []byte("public prompt"))
		answer = answer || bytes.Contains(record.Data, []byte("safe answer"))
		var ev chatstream.Event
		_ = json.Unmarshal(record.Data, &ev)
		if ev.RunID == receipt["run_id"] && ev.IsTerminal() {
			terminals++
		}
	}
	if count == 0 || terminals != 1 || !public || !answer {
		t.Fatalf("persisted log: records=%d terminals=%d public=%t answer=%t", count, terminals, public, answer)
	}
}
