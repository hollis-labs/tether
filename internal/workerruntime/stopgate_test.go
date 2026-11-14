package workerruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/libs/security/secrets/tokenfile"
)

// gateHub answers the three MCP steps the gate sends and records their order.
type gateHub struct {
	mu      sync.Mutex
	methods []string
	as      string
	result  string // tools/call result text; "" means pending:[]
	status  int    // non-zero: refuse tools/call with this status
}

func (h *gateHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+tokenOne {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if r.Method == http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed) // no standalone stream
		return
	}
	if r.Method == http.MethodDelete {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	body, _ := io.ReadAll(r.Body)
	var m struct {
		Method string          `json:"method"`
		ID     json.RawMessage `json:"id"`
		Params struct {
			Name      string            `json:"name"`
			Arguments map[string]string `json:"arguments"`
		} `json:"params"`
	}
	_ = json.Unmarshal(body, &m)
	h.mu.Lock()
	h.methods = append(h.methods, m.Method)
	h.mu.Unlock()
	switch m.Method {
	case "initialize":
		w.Header().Set("Mcp-Session-Id", "sess-1")
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2025-06-18","capabilities":{},"serverInfo":{"name":"hub","version":"1"}}}`, m.ID)
	case "tools/call":
		if h.status != 0 {
			w.WriteHeader(h.status)
			return
		}
		h.mu.Lock()
		h.as = m.Params.Arguments["as"]
		h.mu.Unlock()
		text := h.result
		if text == "" {
			text = `{"handle":"@me","pending":[]}`
		}
		quoted, _ := json.Marshal(text)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":%s}]}}`, m.ID, quoted)
	default:
		w.WriteHeader(http.StatusAccepted)
	}
}

func runGate(t *testing.T, hub *gateHub, payload string) (string, string) {
	t.Helper()
	return runGateState(t, hub, payload, filepath.Join(t.TempDir(), "stop-gate.state"))
}

func runGateState(t *testing.T, hub *gateHub, payload, state string) (string, string) {
	t.Helper()
	srv := httptest.NewServer(hub)
	t.Cleanup(srv.Close)
	file := filepath.Join(t.TempDir(), "token")
	if err := tokenfile.Write(file, tokenOne); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	RunStopGate(context.Background(), strings.NewReader(payload), &out, &errb, srv.URL, file, "msg://agent/agent-mux/agt_me", state)
	return out.String(), errb.String()
}

func TestStopGateBlocksOnceWhileARepliesWait(t *testing.T) {
	hub := &gateHub{result: `{"handle":"@me","pending":[{"message_id":"m1","from":"msg://agent/agent-mux/agt_archie","kind":"question","subject":"Ship it?"}]}`}
	out, _ := runGate(t, hub, `{"hook_event_name":"Stop","stop_hook_active":false}`)
	var d struct{ Decision, Reason string }
	if err := json.Unmarshal([]byte(out), &d); err != nil || d.Decision != "block" || !strings.Contains(d.Reason, "m1 question") || !strings.Contains(d.Reason, "tether_message_post") {
		t.Fatalf("decision = %q (%v)", out, err)
	}
	if strings.Join(hub.methods, ",") != "initialize,notifications/initialized,tools/call" || hub.as != "msg://agent/agent-mux/agt_me" {
		t.Fatalf("hub saw %v as %q", hub.methods, hub.as)
	}
	// The stop that follows our own block is never gated, and the hub is not asked.
	hub2 := &gateHub{result: hub.result}
	if out, _ := runGate(t, hub2, `{"stop_hook_active":true}`); out != "" || len(hub2.methods) != 0 {
		t.Fatalf("gated a continuation stop: %q %v", out, hub2.methods)
	}
}

func TestStopGateLetsTheTurnEndWhenNothingWaitsOrOnFailure(t *testing.T) {
	if out, _ := runGate(t, &gateHub{}, `{"stop_hook_active":false}`); out != "" {
		t.Fatalf("blocked with nothing pending: %q", out)
	}
	out, errOut := runGate(t, &gateHub{status: http.StatusInternalServerError}, `{"stop_hook_active":false}`)
	if out != "" || !strings.Contains(errOut, "not gating") || strings.Contains(errOut, tokenOne) {
		t.Fatalf("failure: out=%q err=%q", out, errOut)
	}
	if out, errOut := runGate(t, &gateHub{}, `not json`); out != "" || !strings.Contains(errOut, "not gating") {
		t.Fatalf("bad payload: out=%q err=%q", out, errOut)
	}
	var b bytes.Buffer
	RunStopGate(context.Background(), strings.NewReader(`{}`), &b, io.Discard, "http://example.com/mcp", "/nonexistent", "", "")
	if b.Len() != 0 {
		t.Fatalf("non-loopback http url gated: %q", b.String())
	}
}

func TestStopGateReasonCapsTheList(t *testing.T) {
	var pending []pendingReply
	for i := 0; i < 8; i++ {
		pending = append(pending, pendingReply{MessageID: fmt.Sprintf("m%d", i), Kind: "question", From: "x"})
	}
	r := stopGateReason(pending)
	if !strings.Contains(r, "8 typed message(s)") || !strings.Contains(r, "and 3 more") || strings.Contains(r, "m6 ") {
		t.Fatalf("reason = %q", r)
	}
}

func pendingJSON(items ...pendingReply) string {
	raw, _ := json.Marshal(map[string]any{"handle": "@me", "pending": items})
	return string(raw)
}

// Each pending message blocks at most one stop; a new one blocks again; one older than the window never blocks.
func TestStopGateBlocksOncePerItemWithinTheWindow(t *testing.T) {
	state := filepath.Join(t.TempDir(), "stop-gate.state")
	now := time.Now().UTC()
	m1 := pendingReply{MessageID: "m1", Kind: "question", From: "x", CreatedAt: now.Add(-time.Hour)}
	m2 := pendingReply{MessageID: "m2", Kind: "decision", From: "x", CreatedAt: now.Add(-time.Minute)}
	old := pendingReply{MessageID: "old", Kind: "question", From: "x", CreatedAt: now.Add(-StopGateWindow - time.Hour)}
	stop := `{"stop_hook_active":false}`

	out, _ := runGateState(t, &gateHub{result: pendingJSON(old, m1)}, stop, state)
	if !strings.Contains(out, `"block"`) || !strings.Contains(out, "m1 question") || strings.Contains(out, "old question") {
		t.Fatalf("first stop: %q", out)
	}
	if out, _ := runGateState(t, &gateHub{result: pendingJSON(old, m1)}, stop, state); out != "" {
		t.Fatalf("m1 blocked a second stop: %q", out)
	}
	out, _ = runGateState(t, &gateHub{result: pendingJSON(old, m1, m2)}, stop, state)
	if !strings.Contains(out, "m2 decision") || strings.Contains(out, "m1 question") {
		t.Fatalf("new item: %q", out)
	}
	if raw, _ := os.ReadFile(state); string(raw) != "m1\nm2\n" {
		t.Fatalf("state = %q", raw)
	}
}

// A state file that cannot be written means no block: never nag without a memory.
func TestStopGateWithoutStateDoesNotBlock(t *testing.T) {
	m1 := pendingReply{MessageID: "m1", Kind: "question", From: "x", CreatedAt: time.Now()}
	out, errOut := runGateState(t, &gateHub{result: pendingJSON(m1)}, `{"stop_hook_active":false}`, "")
	if out != "" || !strings.Contains(errOut, "not gating") {
		t.Fatalf("out=%q err=%q", out, errOut)
	}
}

// Any panic in the gate is recovered: nothing on stdout, the call returns (the hook exits 0).
func TestStopGatePanicFailsOpen(t *testing.T) {
	prev := askPending
	askPending = func(context.Context, *HTTPRelay, string) ([]pendingReply, error) { panic("boom") }
	t.Cleanup(func() { askPending = prev })
	var out, errb bytes.Buffer
	RunStopGate(context.Background(), strings.NewReader(`{"stop_hook_active":false}`), &out, &errb, "https://hub.example/mcp", "/x", "", filepath.Join(t.TempDir(), "s"))
	if out.Len() != 0 || !strings.Contains(errb.String(), "internal error (boom); not gating") {
		t.Fatalf("out=%q err=%q", out.String(), errb.String())
	}
}
