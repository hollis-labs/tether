package codexappserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/libs/ui/ui-go/chatstream"
	"github.com/hollis-labs/tether/internal/chatbackend"
)

const start = `{"method":"turn/started","params":{"threadId":"thread-1","turn":{"id":"turn-1","status":"inProgress"}}}`

func completed(status string) string {
	return fmt.Sprintf(`{"method":"turn/completed","params":{"threadId":"thread-1","turn":{"id":"turn-1","status":%q,"error":{"message":"scripted failure"}}}}`, status)
}
func item(method, body string) string {
	return fmt.Sprintf(`{"method":%q,"params":{"threadId":"thread-1","turnId":"turn-1","item":%s}}`, method, body)
}
func delta(method, id, text string) string {
	return fmt.Sprintf(`{"method":%q,"params":{"threadId":"thread-1","turnId":"turn-1","itemId":%q,"delta":%q,"summaryIndex":0}}`, method, id, text)
}

// fakeServer scripts the same bidirectional frames as the sole session reader.
// Notifications may arrive before Send returns, just as they do before the RPC
// turn/start response. No decoder, request path or terminal is stubbed out.
type fakeServer struct {
	tap       *Tap
	mu        sync.Mutex
	inputs    []string
	script    []string
	interrupt chan [2]string
}

func (f *fakeServer) write(lines ...string) {
	for _, s := range lines {
		_, _ = f.tap.Write([]byte(s + "\n"))
	}
}
func (f *fakeServer) send(_ context.Context, text string) error {
	f.mu.Lock()
	f.inputs = append(f.inputs, text)
	f.mu.Unlock()
	f.write(f.script...)
	return nil
}
func setup(t *testing.T, script ...string) (*Backend, *fakeServer) {
	t.Helper()
	f := &fakeServer{tap: NewTap(), script: script, interrupt: make(chan [2]string, 4)}
	b := New(Config{Tap: f.tap, Send: f.send, CancelGrace: 75 * time.Millisecond, Interrupt: func(_ context.Context, thread, turn string) error { f.interrupt <- [2]string{thread, turn}; return nil }})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- b.Run(ctx) }()
	<-b.Ready()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("backend did not stop")
		}
	})
	return b, f
}
func collect(t *testing.T, r chatbackend.Run, want chatstream.Verb) []chatstream.Event {
	t.Helper()
	out := make(chan []chatstream.Event, 1)
	go func() {
		var evs []chatstream.Event
		for e, err := range r.Events() {
			if err != nil {
				return
			}
			evs = append(evs, e)
		}
		out <- evs
	}()
	select {
	case evs := <-out:
		n := 0
		for _, e := range evs {
			if e.IsTerminal() {
				n++
			}
			if e.RunID != r.ID() {
				t.Errorf("run id %s != %s", e.RunID, r.ID())
			}
		}
		if n != 1 || len(evs) == 0 || evs[0].Verb != chatstream.VerbRunStart || evs[len(evs)-1].Verb != want {
			t.Fatalf("invalid run (%d terminals): %+v", n, evs)
		}
		return evs
	case <-time.After(3 * time.Second):
		t.Fatal("run did not terminate")
		return nil
	}
}
func submit(t *testing.T, b *Backend) chatbackend.Run {
	t.Helper()
	r, err := b.Submit(context.Background(), chatbackend.Turn{RunID: "run-test", Text: "hello", Context: json.RawMessage(`{ "secret":"private-marker" }`)})
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func TestScriptedServer(t *testing.T) {
	tests := []struct {
		name   string
		frames []string
		verb   chatstream.Verb
		kind   chatstream.PartKind
	}{
		{"text deltas", []string{item("item/started", `{"id":"answer","type":"agentMessage","text":""}`), delta("item/agentMessage/delta", "answer", "hello"), item("item/completed", `{"id":"answer","type":"agentMessage","text":"hello"}`)}, chatstream.VerbRunFinish, chatstream.PartText},
		{"reasoning", []string{item("item/started", `{"id":"reason","type":"reasoning","summary":[]}`), delta("item/reasoning/summaryTextDelta", "reason", "thinking"), item("item/completed", `{"id":"reason","type":"reasoning","summary":["thinking"]}`)}, chatstream.VerbRunFinish, chatstream.PartReasoning},
		{"command execution", []string{item("item/started", `{"id":"cmd","type":"commandExecution","command":"pwd","cwd":"/work","status":"inProgress"}`), item("item/completed", `{"id":"cmd","type":"commandExecution","command":"pwd","cwd":"/work","status":"completed","aggregatedOutput":"/work","exitCode":0}`)}, chatstream.VerbRunFinish, chatstream.PartToolResult},
		{"mcp tool call", []string{item("item/started", `{"id":"tool","type":"mcpToolCall","server":"app","tool":"read","arguments":{"key":"value"},"status":"inProgress"}`), item("item/completed", `{"id":"tool","type":"mcpToolCall","server":"app","tool":"read","arguments":{"key":"value"},"status":"completed","result":{"content":[{"type":"text","text":"result"}]}}`)}, chatstream.VerbRunFinish, chatstream.PartToolResult},
		{"interrupted", nil, chatstream.VerbRunAbort, ""},
		{"failed", nil, chatstream.VerbRunError, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			status := "completed"
			if tc.name == "interrupted" {
				status = "interrupted"
			}
			if tc.name == "failed" {
				status = "failed"
			}
			script := []string{start, item("item/started", `{"id":"user","type":"userMessage","content":[{"type":"text","text":"<app-context>private-marker</app-context>hello"}]}`), `{"id":42,"result":{"turn":{"items":[{"text":"private-marker"}]}}}`}
			script = append(script, `{"method":"thread/snapshot","params":{"threadId":"thread-1","preview":"private-marker"}}`)
			script = append(script, tc.frames...)
			script = append(script, completed(status), completed(status))
			b, f := setup(t, script...)
			if !b.Info().Deltas || b.Info().Approvals {
				t.Fatal("wrong capabilities")
			}
			r := submit(t, b)
			evs := collect(t, r, tc.verb)
			f.mu.Lock()
			input := f.inputs[0]
			f.mu.Unlock()
			if input != "<app-context>{\"secret\":\"private-marker\"}</app-context>\nhello" {
				t.Fatalf("input = %q", input)
			}
			raw, _ := json.Marshal(evs)
			if strings.Contains(string(raw), "private-marker") {
				t.Fatal("private context in public events")
			}
			if tc.kind != "" {
				found := false
				for _, e := range evs {
					if e.Verb == chatstream.VerbPartStart && e.PartKind() == tc.kind {
						found = true
					}
				}
				if !found {
					t.Fatalf("no %s part", tc.kind)
				}
			}
		})
	}
}
func TestBusyAndMultiTurn(t *testing.T) {
	b, f := setup(t, start)
	first := submit(t, b)
	if _, err := b.Submit(context.Background(), chatbackend.Turn{Text: "second"}); !errors.Is(err, chatbackend.ErrBusy) {
		t.Fatalf("busy = %v", err)
	}
	f.mu.Lock()
	n := len(f.inputs)
	f.mu.Unlock()
	if n != 1 {
		t.Fatal("busy input delivered")
	}
	f.write(completed("completed"))
	collect(t, first, chatstream.VerbRunFinish)
	second := submit(t, b)
	f.write(completed("completed"))
	collect(t, second, chatstream.VerbRunFinish)
	f.mu.Lock()
	n = len(f.inputs)
	f.mu.Unlock()
	if n != 2 {
		t.Fatalf("%d inputs", n)
	}
}
func TestObservedTurn(t *testing.T) {
	for _, noted := range []bool{true, false} {
		t.Run(fmt.Sprint(noted), func(t *testing.T) {
			b, f := setup(t)
			write := func() error {
				f.write(start, item("item/started", `{"type":"userMessage","content":[{"type":"text","text":"mail wake"}]}`), delta("item/agentMessage/delta", "a", "observed"), completed("completed"))
				return nil
			}
			if noted {
				if err := f.tap.Input("mail wake", "send_turn", false, write); err != nil {
					t.Fatal(err)
				}
			} else {
				_ = write()
			}
			select {
			case o := <-b.Observed():
				if o.Text != "mail wake" || o.TurnRef != "turn-1" {
					t.Fatalf("observed %+v", o)
				}
				collect(t, o.Run, chatstream.VerbRunFinish)
			case <-time.After(time.Second):
				t.Fatal("no observed turn")
			}
		})
	}
}
func TestCancelTargetsTurnAndStaysBusy(t *testing.T) {
	b, f := setup(t, start)
	r := submit(t, b)
	r.Cancel()
	r.Cancel()
	select {
	case target := <-f.interrupt:
		if target != [2]string{"thread-1", "turn-1"} {
			t.Fatalf("interrupt %v", target)
		}
	case <-time.After(time.Second):
		t.Fatal("no interrupt")
	}
	collect(t, r, chatstream.VerbRunAbort)
	if _, err := b.Submit(context.Background(), chatbackend.Turn{Text: "new"}); !errors.Is(err, chatbackend.ErrBusy) {
		t.Fatalf("active tombstone: %v", err)
	}
	f.write(completed("interrupted"))
	r.Cancel()
	select {
	case <-f.interrupt:
		t.Fatal("duplicate interrupt")
	default:
	}
	r = submit(t, b)
	r.Cancel()
	f.write(completed("completed"))
	collect(t, r, chatstream.VerbRunAbort)
}
func TestInvalidContextAndSendFailure(t *testing.T) {
	b, f := setup(t)
	if _, err := b.Submit(context.Background(), chatbackend.Turn{Text: "x", Context: json.RawMessage(`{`)}); err == nil {
		t.Fatal("invalid context accepted")
	}
	f.mu.Lock()
	n := len(f.inputs)
	f.mu.Unlock()
	if n != 0 {
		t.Fatal("invalid input delivered")
	}
	b.cfg.Send = func(context.Context, string) error { return errors.New("write failed") }
	if _, err := b.Submit(context.Background(), chatbackend.Turn{Text: "x"}); err == nil {
		t.Fatal("write error missing")
	}
	b.cfg.Send = f.send
	f.script = []string{start, completed("completed")}
	collect(t, submit(t, b), chatstream.VerbRunFinish)
}
func TestAttachMidTurnAndSessionExit(t *testing.T) {
	tap := NewTap()
	_, _ = tap.Write([]byte(start + "\n"))
	b := New(Config{Tap: tap, Send: func(context.Context, string) error { t.Error("mid-turn submit delivered"); return nil }})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- b.Run(ctx) }()
	<-b.Ready()
	if _, err := b.Submit(ctx, chatbackend.Turn{Text: "x"}); !errors.Is(err, chatbackend.ErrBusy) {
		t.Fatalf("mid-turn busy %v", err)
	}
	_, _ = tap.Write([]byte(delta("item/agentMessage/delta", "a", "partial") + "\n"))
	o := <-b.Observed()
	tap.Close()
	collect(t, o.Run, chatstream.VerbRunError)
	<-done
	if _, err := b.Submit(ctx, chatbackend.Turn{Text: "x"}); !errors.Is(err, chatbackend.ErrClosed) {
		t.Fatalf("closed %v", err)
	}
}
func TestApprovalRequestsAreActivityOnly(t *testing.T) {
	b, _ := setup(t, start, `{"id":9,"method":"item/commandExecution/requestApproval","params":{"threadId":"thread-1","turnId":"turn-1","itemId":"cmd","command":"pwd"}}`, completed("completed"))
	evs := collect(t, submit(t, b), chatstream.VerbRunFinish)
	found := false
	for _, e := range evs {
		if e.Verb == chatstream.VerbActivity {
			found = true
		}
		if strings.HasPrefix(string(e.Verb), "approval.") {
			t.Fatal("approval bridge enabled")
		}
	}
	if !found {
		t.Fatal("missing approval activity")
	}
}

func TestCancelBeforeTurnStarts(t *testing.T) {
	b, f := setup(t)
	r := submit(t, b)
	r.Cancel()
	collect(t, r, chatstream.VerbRunAbort)
	if _, err := b.Submit(context.Background(), chatbackend.Turn{Text: "second"}); !errors.Is(err, chatbackend.ErrBusy) {
		t.Fatalf("pending canceled turn: %v", err)
	}
	f.write(start)
	select {
	case got := <-f.interrupt:
		if got != [2]string{"thread-1", "turn-1"} {
			t.Fatalf("target %v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("late started turn not interrupted")
	}
	f.write(completed("interrupted"))
	select {
	case <-b.Observed():
		t.Fatal("canceled submitted turn became observed")
	default:
	}
}
func TestClaimTimeoutAndLateObservedTurn(t *testing.T) {
	b, f := setup(t)
	b.cfg.ClaimTimeout = 25 * time.Millisecond
	r := submit(t, b)
	collect(t, r, chatstream.VerbRunError)
	f.write(start, completed("completed"))
	select {
	case o := <-b.Observed():
		if o.Text != "hello" {
			t.Fatalf("text %q", o.Text)
		}
		collect(t, o.Run, chatstream.VerbRunFinish)
	case <-time.After(time.Second):
		t.Fatal("late input lost")
	}
}
func TestObservedCompletedBeforeEOFIsDrained(t *testing.T) {
	b, f := setup(t)
	f.write(start, item("item/started", `{"type":"userMessage","content":[{"type":"text","text":"terminal input"}]}`), completed("completed"))
	f.tap.Close()
	select {
	case o := <-b.Observed():
		collect(t, o.Run, chatstream.VerbRunFinish)
	case <-time.After(time.Second):
		t.Fatal("completed observed run lost on EOF")
	}
}

func TestObservedPrivateInputBeforeAttach(t *testing.T) {
	for _, active := range []bool{false, true} {
		t.Run(fmt.Sprint(active), func(t *testing.T) {
			tap := NewTap()
			text := "<app-context>{\"private\":\"reattach-secret\"}</app-context>\npublic input"
			if err := tap.Input(text, "send_turn", false, func() error { return nil }); err != nil {
				t.Fatal(err)
			}
			if active {
				_, _ = tap.Write([]byte(start + "\n"))
			}
			b := New(Config{Tap: tap})
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- b.Run(ctx) }()
			<-b.Ready()
			t.Cleanup(func() { cancel(); <-done })
			f := &fakeServer{tap: tap}
			if !active {
				f.write(start)
			}
			f.write(item("item/started", `{"type":"userMessage","content":[{"type":"text","text":"<app-context>reattach-secret</app-context>\npublic input"}]}`), delta("item/agentMessage/delta", "a", "reply"), completed("completed"))
			select {
			case o := <-b.Observed():
				if o.Text != "public input" {
					t.Fatalf("observed input %q", o.Text)
				}
				evs := collect(t, o.Run, chatstream.VerbRunFinish)
				raw, _ := json.Marshal(evs)
				if bytes.Contains(raw, []byte("reattach-secret")) {
					t.Fatal("private input reflected after attach")
				}
			case <-time.After(time.Second):
				t.Fatal("reattached input lost")
			}
		})
	}
}

// A recovered controller's cursor can sit in the middle of a line. Restore
// that prefix without replaying a committed user message or losing completion.
func TestRestoredTurnBoundaryAndPartialLine(t *testing.T) {
	for _, userEcho := range []bool{false, true} {
		t.Run(fmt.Sprint(userEcho), func(t *testing.T) {
			line := completed("completed")
			if userEcho {
				line = item("item/started", `{"type":"userMessage","content":[{"type":"text","text":"<app-context>restore-secret</app-context>\npublic input"}]}`)
			}
			split := len(line) / 2
			tap := NewTap()
			tap.Restore("thread-1", "turn-1", []byte(line[:split]))
			f := &fakeServer{tap: tap}
			b := New(Config{Tap: tap, Send: f.send})
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- b.Run(ctx) }()
			<-b.Ready()
			t.Cleanup(func() { cancel(); <-done })
			if _, err := b.Submit(ctx, chatbackend.Turn{Text: "busy"}); !errors.Is(err, chatbackend.ErrBusy) {
				t.Fatalf("recovered busy %v", err)
			}
			if len(f.inputs) != 0 {
				t.Fatal("recovered busy input delivered")
			}
			f.write(line[split:])
			if userEcho {
				f.write(completed("completed"))
			}
			select {
			case o := <-b.Observed():
				if userEcho && o.Text != "public input" {
					t.Fatalf("recovered input %q", o.Text)
				}
				raw, _ := json.Marshal(collect(t, o.Run, chatstream.VerbRunFinish))
				if bytes.Contains(raw, []byte("restore-secret")) {
					t.Fatal("restored private partial line leaked")
				}
			case <-time.After(time.Second):
				t.Fatal("restored completion lost")
			}
		})
	}
}
