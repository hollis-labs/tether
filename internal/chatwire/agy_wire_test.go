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

// echoAgySessions supplies real agystdio decoding while the provider deliberately
// echoes private input through both a recognized user frame and an unknown frame.
type echoAgySessions struct{ inputs chan string }

func (*echoAgySessions) ChatRuntime(string) (Runtime, error) { return RuntimeAgyStdio, nil }
func (s *echoAgySessions) ChatBackend(string, Runtime) (Backend, error) {
	return agystdio.New(agystdio.Config{Executor: s}), nil
}
func (s *echoAgySessions) StartTurn(_ context.Context, input string, line func([]byte)) (func() error, error) {
	s.inputs <- input
	return func() error {
		line([]byte(`{"event":"init","conversation_id":"native-echo"}`))
		echo, _ := json.Marshal(map[string]any{"event": "step_update", "step_update": map[string]any{"step_index": 0, "step_type": "user_input", "state": "DONE", "text_delta": input}})
		line(echo)
		unknown, _ := json.Marshal(map[string]any{"event": "unknown_echo", "input": input, "context": map[string]string{"private": "wire-private-context"}})
		line(unknown)
		line([]byte(`{"event":"step_update","step_update":{"step_index":1,"step_type":"agent_response","state":"DONE","text_delta":"safe answer"}}`))
		line([]byte(`{"event":"result","result":{"status":"SUCCESS","response":"safe answer"}}`))
		return nil
	}, nil
}

func TestAgyWireContextEchoExcludedFromEventLog(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := tstore.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	(&env{st: st}).session(t, "agy-session", "agy-seat", "")
	sessions := &echoAgySessions{inputs: make(chan string, 1)}
	w, err := Open(ctx, Config{StateDir: dir, Store: st, Sessions: sessions})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(api.NewHandler(api.Deps{ChatWire: w.Handler()}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { srv.CloseClientConnections(); _ = w.Close(context.Background()) })
	c := client{t: t, base: srv.URL}
	status, conv := c.do(http.MethodPost, "/chat/v1/agents/agy-seat/conversation", nil)
	if status != http.StatusOK {
		t.Fatalf("conversation = %d %v", status, conv)
	}
	convID := conv["conversation_id"].(string)
	status, attached := c.do(http.MethodPost, "/chat/v1/conversations/"+convID+"/attachments", map[string]string{"role": "controller", "stream": "deltas"})
	if status != http.StatusCreated {
		t.Fatalf("attach = %d %v", status, attached)
	}
	attachment := attached["attachment_id"].(string)
	stream, status := c.stream(convID, attachment, "0")
	if status != http.StatusOK {
		t.Fatalf("stream = %d", status)
	}
	status, receipt := c.do(http.MethodPost, "/chat/v1/conversations/"+convID+"/turns", map[string]any{"attachment": attachment, "text": "public question", "request_id": "agy-context", "context": json.RawMessage(`{"private":"wire-private-context"}`)})
	if status != http.StatusAccepted {
		t.Fatalf("submit = %d %v", status, receipt)
	}
	for _, event := range until(t, stream, func(ev chatstream.Event) bool { return ev.IsTerminal() }) {
		if strings.Contains(event.Raw, "wire-private-context") || strings.Contains(event.Raw, "app-context") {
			t.Fatalf("private context streamed: %s", event.Raw)
		}
	}
	select {
	case input := <-sessions.inputs:
		if input != "<app-context>{\"private\":\"wire-private-context\"}</app-context>\npublic question" {
			t.Fatalf("agy input = %q", input)
		}
	case <-time.After(time.Second):
		t.Fatal("agy received no input")
	}
	terminals, records := 0, 0
	answer := false
	for rec, err := range w.db.Log.After(ctx, convID, 0) {
		if err != nil {
			t.Fatal(err)
		}
		records++
		if bytes.Contains(rec.Data, []byte("wire-private-context")) || bytes.Contains(rec.Data, []byte("app-context")) {
			t.Fatalf("private context persisted: %s", rec.Data)
		}
		var ev chatstream.Event
		if err := json.Unmarshal(rec.Data, &ev); err != nil {
			t.Fatal(err)
		}
		if ev.RunID == receipt["run_id"] {
			if ev.IsTerminal() {
				terminals++
				if ev.Verb != chatstream.VerbRunFinish {
					t.Fatalf("unexpected terminal: %s", rec.Data)
				}
			}
			answer = answer || (ev.Verb == chatstream.VerbPartEnd && bytes.Contains(ev.Final, []byte("safe answer")))
		}
	}
	if records == 0 || terminals != 1 || !answer {
		t.Fatalf("persisted records=%d terminals=%d safe answer=%v", records, terminals, answer)
	}
}
