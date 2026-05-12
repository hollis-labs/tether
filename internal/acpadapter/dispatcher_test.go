package acpadapter

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestDispatcher_RequestRoundTrip drives one inbound request and
// verifies the dispatcher writes a properly-shaped response.
func TestDispatcher_RequestRoundTrip(t *testing.T) {
	in := strings.NewReader(`{"jsonrpc":"2.0","id":42,"method":"echo","params":{"text":"hi"}}` + "\n")
	var out syncBuffer
	d := NewDispatcher(NewReader(in), NewWriter(&out))
	d.HandleMethod("echo", func(_ context.Context, params json.RawMessage) (any, error) {
		var p struct{ Text string }
		_ = json.Unmarshal(params, &p)
		return map[string]string{"echo": p.Text}, nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := d.Run(ctx)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("Run: want io.EOF (clean stdin close), got %v", err)
	}

	resp := out.parse(t)
	if !resp.IsResponse() {
		t.Fatalf("not a response: %+v", resp)
	}
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	var body map[string]string
	_ = json.Unmarshal(resp.Result, &body)
	if body["echo"] != "hi" {
		t.Errorf("echo body = %v, want {echo:hi}", body)
	}
}

func TestDispatcher_NotificationDoesNotRespond(t *testing.T) {
	in := strings.NewReader(`{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"s1"}}` + "\n")
	var out syncBuffer
	d := NewDispatcher(NewReader(in), NewWriter(&out))
	got := make(chan string, 1)
	d.HandleNotification("session/cancel", func(_ context.Context, params json.RawMessage) {
		var p struct {
			SessionID string `json:"sessionId"`
		}
		_ = json.Unmarshal(params, &p)
		got <- p.SessionID
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = d.Run(ctx)

	select {
	case sid := <-got:
		if sid != "s1" {
			t.Errorf("notif sessionId = %q, want s1", sid)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("notification handler not called")
	}
	if out.Len() != 0 {
		t.Errorf("notification produced output (notifications must not respond): %q", out.String())
	}
}

func TestDispatcher_UnknownMethodReturnsMethodNotFound(t *testing.T) {
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"bogus"}` + "\n")
	var out syncBuffer
	d := NewDispatcher(NewReader(in), NewWriter(&out))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = d.Run(ctx)

	resp := out.parse(t)
	if resp.Error == nil {
		t.Fatal("expected error response")
	}
	if resp.Error.Code != ErrCodeMethodNotFound {
		t.Errorf("Code = %d, want %d", resp.Error.Code, ErrCodeMethodNotFound)
	}
}

func TestDispatcher_HandlerReturnsRPCError(t *testing.T) {
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"x"}` + "\n")
	var out syncBuffer
	d := NewDispatcher(NewReader(in), NewWriter(&out))
	d.HandleMethod("x", func(_ context.Context, _ json.RawMessage) (any, error) {
		return nil, &RPCError{Code: ErrCodeInvalidParams, Message: "bad"}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = d.Run(ctx)

	resp := out.parse(t)
	if resp.Error == nil || resp.Error.Code != ErrCodeInvalidParams {
		t.Fatalf("expected invalid_params error, got %+v", resp.Error)
	}
}

// TestDispatcher_OutboundCall verifies the bidirectional path: handler
// calls dispatcher.Call() to send an outbound request, peer responds,
// dispatcher correlates the response.
func TestDispatcher_OutboundCall(t *testing.T) {
	// Two pipes simulate a peer connection: we (the test) play the
	// "editor" reading the dispatcher's outbound bytes, then write a
	// response back so the dispatcher's Call() returns.
	dispatcherIn, editorOut := io.Pipe() // editor → dispatcher
	editorIn, dispatcherOut := io.Pipe() // dispatcher → editor
	d := NewDispatcher(NewReader(dispatcherIn), NewWriter(dispatcherOut))

	// Editor goroutine: read whatever the dispatcher writes; if it's
	// an outbound request, reply.
	editorReader := NewReader(editorIn)
	go func() {
		for {
			msg, err := editorReader.Read()
			if err != nil {
				return
			}
			if msg.IsRequest() {
				resp := &Message{ID: msg.ID, Result: json.RawMessage(`{"ok":true}`)}
				_ = NewWriter(editorOut).Write(resp)
			}
		}
	}()

	// Trigger Call() from a goroutine; Run() drives the response back in.
	type result struct {
		raw json.RawMessage
		err error
	}
	done := make(chan result, 1)
	go func() {
		raw, err := d.Call(context.Background(), "fs/read_text_file", map[string]string{"path": "/x"})
		done <- result{raw, err}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() {
		_ = d.Run(ctx)
	}()

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("Call returned error: %v", r.err)
		}
		if !strings.Contains(string(r.raw), `"ok":true`) {
			t.Errorf("Call result = %s, want ok:true", r.raw)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Call did not return")
	}
	cancel()
	// Close pipes so Run exits.
	_ = dispatcherIn.Close()
	_ = dispatcherOut.Close()
}

// syncBuffer is a thread-safe bytes.Buffer for capturing dispatcher
// output across goroutines without data races.
type syncBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf = append(s.buf, p...)
	return len(p), nil
}

func (s *syncBuffer) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.buf)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return string(s.buf)
}

// parse extracts the first JSON-RPC message from the buffer.
func (s *syncBuffer) parse(t *testing.T) *Message {
	t.Helper()
	r := NewReader(strings.NewReader(s.String()))
	msg, err := r.Read()
	if err != nil {
		t.Fatalf("parse buffer: %v\nbuffer:\n%s", err, s.String())
	}
	return msg
}
