//go:build !windows

package shimcodex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

type memoryStore struct {
	mu          sync.Mutex
	state       State
	commitError error
}

func (m *memoryStore) Load(context.Context) (State, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state.Version == "" {
		return State{}, ErrMissing
	}
	return clone(m.state), nil
}
func (m *memoryStore) Commit(ctx context.Context, previous uint64, s State) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.commitError != nil {
		return m.commitError
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if m.state.Revision != previous {
		return errors.New("CAS conflict")
	}
	m.state = clone(s)
	return nil
}
func newEngine(t *testing.T) (*Engine, *memoryStore) {
	t.Helper()
	m := &memoryStore{}
	e, err := Open(context.Background(), m, Binding{Session: "s", Instance: "i", Generation: 1, Operation: "p", Journal: "j", Attempt: "fixture-attempt", Fingerprint: "fixture-fingerprint"}, 1, true, Limits{InboxItems: 64, InboxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	return e, m
}
func reserve(t *testing.T, e *Engine, method, params string) Operation {
	t.Helper()
	op, err := e.Reserve(context.Background(), e.Snapshot().Epoch, method, json.RawMessage(params))
	if err != nil {
		t.Fatal(err)
	}
	return op
}
func attempted(t *testing.T, e *Engine, op Operation) {
	t.Helper()
	if err := e.Attempt(context.Background(), e.Snapshot().Epoch, op.ID); err != nil {
		t.Fatal(err)
	}
}
func receive(t *testing.T, e *Engine, position int, raw string) {
	t.Helper()
	ok, err := e.AcceptOutput(context.Background(), e.Snapshot().Epoch, fmt.Sprintf("j:%d", position), "stdout", []byte(raw+"\n"))
	if err != nil || !ok {
		t.Fatalf("accept=%v err=%v", ok, err)
	}
}
func handshake(t *testing.T, e *Engine) {
	t.Helper()
	init := reserve(t, e, "initialize", `{"clientInfo":{"name":"fixture","version":"1"}}`)
	attempted(t, e, init)
	receive(t, e, 1, fmt.Sprintf(`{"id":%d,"result":{"userAgent":"fixture"}}`, init.ID))
	notify := reserve(t, e, "initialized", `{}`)
	attempted(t, e, notify)
	if err := e.BytesWritten(context.Background(), e.Snapshot().Epoch, notify.ID); err != nil {
		t.Fatal(err)
	}
}
func boundThread(t *testing.T, e *Engine) {
	t.Helper()
	handshake(t, e)
	op := reserve(t, e, "thread/start", `{}`)
	attempted(t, e, op)
	receive(t, e, 2, fmt.Sprintf(`{"id":%d,"result":{"thread":{"id":"native-t"}}}`, op.ID))
}

func TestReconnectNeverReinitializesOrReusesRequestID(t *testing.T) {
	e, m := newEngine(t)
	boundThread(t, e)
	old := reserve(t, e, "turn/start", `{"threadId":"native-t","input":[]}`)
	attempted(t, e, old)
	e, err := Open(context.Background(), m, e.Snapshot().Binding, 2, false, e.limits)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ method, params, code string }{
		{"initialize", `{}`, "already_initialized_or_unknown"}, {"thread/start", `{}`, "thread_exists_or_unknown"}, {"turn/start", `{"threadId":"native-t"}`, "turn_active_or_unknown"},
	} {
		if _, err := e.Reserve(context.Background(), 2, tc.method, json.RawMessage(tc.params)); !HasCode(err, tc.code) {
			t.Fatalf("%s: %v", tc.method, err)
		}
	}
	receive(t, e, 3, fmt.Sprintf(`{"id":%d,"result":{"turn":{"id":"turn-a"}}}`, old.ID))
	receive(t, e, 4, `{"method":"turn/completed","params":{"threadId":"native-t","turn":{"id":"turn-a","status":"completed"}}}`)
	next := reserve(t, e, "turn/start", `{"threadId":"native-t"}`)
	if next.ID <= old.ID {
		t.Fatalf("reused ID: old=%d new=%d", old.ID, next.ID)
	}
	attempted(t, e, next)
	// An old replay must not settle the new pending request.
	receive(t, e, 5, fmt.Sprintf(`{"id":%d,"result":{"turn":{"id":"turn-a"}}}`, old.ID))
	if got := findState(e.Snapshot(), next.ID); got.Phase != Attempted {
		t.Fatalf("old response settled fresh request: %+v", got)
	}
}
func findState(s State, id uint64) Operation { return *find(&s, id) }

func TestAttemptAndCommitFailureRetainUnknown(t *testing.T) {
	e, m := newEngine(t)
	op := reserve(t, e, "initialize", `{}`)
	attempted(t, e, op)
	if err := e.AbandonUnsubmitted(context.Background(), 1, op.ID); !HasCode(err, "outcome_unknown") {
		t.Fatal(err)
	}
	m.commitError = errors.New("ambiguous durable write")
	if err := e.BytesWritten(context.Background(), 1, op.ID); !HasCode(err, "checkpoint_unknown") {
		t.Fatal(err)
	}
	m.commitError = nil
	if _, err := e.Reserve(context.Background(), 1, "initialize", json.RawMessage(`{}`)); !HasCode(err, "checkpoint_unknown") {
		t.Fatal(err)
	}
	e, err := Open(context.Background(), m, e.Snapshot().Binding, 2, false, e.limits)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.Reserve(context.Background(), 2, "initialize", json.RawMessage(`{}`)); !HasCode(err, "already_initialized_or_unknown") {
		t.Fatal(err)
	}
}

func TestDurableChunkSplitAndReplay(t *testing.T) {
	e, m := newEngine(t)
	if _, err := e.AcceptOutput(context.Background(), 1, "j:1", "stdout", []byte(`{"method":"item/agentMessage/delta","params":{"delta":"hel`)); err != nil {
		t.Fatal(err)
	}
	e, err := Open(context.Background(), m, e.Snapshot().Binding, 2, false, e.limits)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.AcceptOutput(context.Background(), 2, "j:2", "stdout", []byte("lo\"}}\n")); err != nil {
		t.Fatal(err)
	}
	s := e.Snapshot()
	if len(s.Inbox) != 1 || len(s.Partial) != 0 || s.Cursor != "j:2" {
		t.Fatalf("lost carry/inbox: %+v", s)
	}
	if ok, err := e.AcceptOutput(context.Background(), 2, "j:2", "stdout", []byte("lo\"}}\n")); ok || err != nil {
		t.Fatalf("replay %v %v", ok, err)
	}
	if got := e.Snapshot(); len(got.Inbox) != 1 || got.Inbox[0].Identity != s.Inbox[0].Identity {
		t.Fatal("duplicate inbox identity")
	}
}

func TestInboxPressureAndFailedCommitNeverAdvanceCursor(t *testing.T) {
	e, m := newEngine(t)
	e.limits.InboxItems = 1
	receive(t, e, 1, `{"method":"one","params":{}}`)
	if _, err := e.AcceptOutput(context.Background(), 1, "j:2", "stdout", []byte("{\"method\":\"two\"}\n")); !HasCode(err, "pressure_retained") {
		t.Fatal(err)
	}
	if e.Snapshot().Cursor != "j:1" {
		t.Fatal("pressure advanced cursor")
	}
	e.limits.InboxItems = 2
	m.commitError = errors.New("write failure")
	if _, err := e.AcceptOutput(context.Background(), 1, "j:2", "stdout", []byte("{\"method\":\"two\"}\n")); !HasCode(err, "checkpoint_unknown") {
		t.Fatal(err)
	}
	if e.Snapshot().Cursor != "j:1" || m.state.Cursor != "j:1" {
		t.Fatal("failed write advanced cursor")
	}
}

func TestServerRequestDoesNotResolveClientOperation(t *testing.T) {
	e, _ := newEngine(t)
	op := reserve(t, e, "initialize", `{}`)
	attempted(t, e, op)
	receive(t, e, 1, fmt.Sprintf(`{"id":%d,"method":"mcpServer/elicitation/request","params":{}}`, op.ID))
	if got := findState(e.Snapshot(), op.ID); got.Phase != Attempted || e.Snapshot().Initialized {
		t.Fatal("server approval resolved client request")
	}
	for _, bad := range []string{`{"id":1,"id":2,"result":{}}`, `{"id":1,"result":{},"error":{"code":1,"message":"bad"}}`, `{"method":"x","result":{}}`, `{"id":null,"result":{}}`, `[]`} {
		if _, err := Decode([]byte(bad)); !HasCode(err, "protocol_invalid") {
			t.Fatalf("accepted malformed RPC %s", bad)
		}
	}
}

func TestTerminalBeforeResponseDoesNotResurrectTurn(t *testing.T) {
	e, _ := newEngine(t)
	boundThread(t, e)
	op := reserve(t, e, "turn/start", `{"threadId":"native-t"}`)
	attempted(t, e, op)
	receive(t, e, 3, `{"method":"turn/started","params":{"threadId":"native-t","turn":{"id":"turn-a"}}}`)
	receive(t, e, 4, `{"method":"turn/completed","params":{"threadId":"native-t","turn":{"id":"turn-a","status":"interrupted"}}}`)
	receive(t, e, 5, fmt.Sprintf(`{"id":%d,"result":{"turn":{"id":"turn-a"}}}`, op.ID))
	if e.Snapshot().ActiveTurn != "" {
		t.Fatal("late response resurrected completed turn")
	}
	if err := e.BytesWritten(context.Background(), 1, op.ID); err != nil {
		t.Fatal(err)
	}
}

func TestOpenRefusesCorruptCommittedLedger(t *testing.T) {
	for _, kind := range []string{"invalid_raw", "reused_id", "unknown_phase", "inbox_pressure", "foreign_cursor", "missing_fingerprint"} {
		t.Run(kind, func(t *testing.T) {
			e, m := newEngine(t)
			boundThread(t, e)
			binding := e.Snapshot().Binding
			m.mu.Lock()
			switch kind {
			case "invalid_raw":
				m.state.Operations[0].Params = json.RawMessage(`broken`)
			case "reused_id":
				m.state.Operations[1].ID = m.state.Operations[0].ID
			case "unknown_phase":
				m.state.Operations[0].Phase = "invented"
			case "inbox_pressure":
				m.state.Inbox = append(m.state.Inbox, Event{Identity: "oversize", Cursor: "j:3", Raw: json.RawMessage(`"` + strings.Repeat("x", 1<<20) + `"`)})
			case "foreign_cursor":
				m.state.Cursor = "other:3"
			case "missing_fingerprint":
				m.state.Binding.Fingerprint = ""
			}
			m.mu.Unlock()
			if _, err := Open(context.Background(), m, binding, 2, false, e.limits); !HasCode(err, "checkpoint_invalid") {
				t.Fatalf("corruption adopted: %v", err)
			}
		})
	}
}

func TestProtocolAdmissionRefusesAmbiguousOrPolicyOverrideParameters(t *testing.T) {
	for _, raw := range []string{
		`{"threadId":"foreign","threadId":"native-t","input":[]}`,
		`{"threadId":"native-t","input":[{"type":"local_image","type":"text","text":"x"}]}`,
		`{"threadId":"native-t","input":[],"approvalPolicy":"never"}`,
		`{"threadId":"native-t","input":[{"type":"local_image","path":"/private/secret"}]}`,
	} {
		e, _ := newEngine(t)
		boundThread(t, e)
		before := e.Snapshot()
		if _, err := e.Reserve(context.Background(), 1, "turn/start", json.RawMessage(raw)); err == nil {
			t.Fatalf("unsupported/ambiguous params admitted: %s", raw)
		}
		after := e.Snapshot()
		if after.Revision != before.Revision || after.NextID != before.NextID || len(after.Operations) != len(before.Operations) {
			t.Fatal("parameter refusal recorded a possible effect")
		}
	}
}

func TestOpenRefusesNonPristineUnboundIntentWithoutCommit(t *testing.T) {
	for _, kind := range []string{"version", "highwater", "terminal_cursor", "server_request", "stream", "turn"} {
		t.Run(kind, func(t *testing.T) {
			binding := Binding{Session: "s", Instance: "i", Generation: 1, Operation: "p", Attempt: "a", Fingerprint: "f"}
			original := State{Version: Version, Binding: binding, Revision: 1, NextID: FirstID}
			switch kind {
			case "version":
				original.Version = "unknown"
			case "highwater":
				original.ReplayHighWater = "j:1"
			case "terminal_cursor":
				original.ExitCursor = "j:1"
			case "server_request":
				original.ServerRequests = []ServerRequest{{Source: "unresolved"}}
			case "stream":
				original.StreamOffset = 1
			case "turn":
				original.ActiveTurn = "prior"
			}
			m := &memoryStore{state: original}
			binding.Journal = "j"
			if _, err := Open(context.Background(), m, binding, 1, true, Limits{InboxItems: 64, InboxBytes: 1 << 20}); !HasCode(err, "checkpoint_invalid") {
				t.Fatalf("non-pristine intent accepted: %v", err)
			}
			got, _ := m.Load(context.Background())
			a, _ := json.Marshal(original)
			b, _ := json.Marshal(got)
			if string(a) != string(b) {
				t.Fatal("invalid intent mutated before validation")
			}
		})
	}
}

// wrappedMissingStore models a store adapter preserving the missing sentinel.
type wrappedMissingStore struct{ memoryStore }

func (m *wrappedMissingStore) Load(context.Context) (State, error) {
	return State{}, fmt.Errorf("adapter: %w", ErrMissing)
}
func TestOpenWrappedMissingIsFreshOnly(t *testing.T) {
	binding := Binding{Session: "s", Instance: "i", Generation: 1, Operation: "p", Journal: "j", Attempt: "a", Fingerprint: "f"}
	for _, fresh := range []bool{false, true} {
		m := &wrappedMissingStore{}
		e, err := Open(context.Background(), m, binding, 1, fresh, Limits{InboxItems: 64, InboxBytes: 1 << 20})
		if fresh {
			if err != nil || e == nil || m.state.Revision != 1 {
				t.Fatalf("fresh missing not initialized: %v", err)
			}
		} else if !errors.Is(err, ErrMissing) || e != nil || m.state.Revision != 0 {
			t.Fatalf("existing missing manufactured ledger: %v", err)
		}
	}
}
