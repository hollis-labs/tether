package workerruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

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
	srv := httptest.NewServer(hub)
	t.Cleanup(srv.Close)
	file := filepath.Join(t.TempDir(), "token")
	if err := tokenfile.Write(file, tokenOne); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	RunStopGate(context.Background(), strings.NewReader(payload), &out, &errb, srv.URL, file, "msg://agent/agent-mux/agt_me")
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
	RunStopGate(context.Background(), strings.NewReader(`{}`), &b, io.Discard, "http://example.com/mcp", "/nonexistent", "")
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
