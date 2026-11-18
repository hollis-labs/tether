package chatwire

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/hollis-labs/libs/harness/core/adapters/agentsessions"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/conversation"
	tstore "github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/terminalattach"
)

// termPTY is the daemon's PTY table for terminal attachments: which sessions have a live PTY, and what was typed.
type termPTY struct {
	mu    sync.Mutex
	live  map[string]bool
	input map[string]string
	sizes map[string][2]uint16
}

func newTermPTY(live ...string) *termPTY {
	p := &termPTY{live: map[string]bool{}, input: map[string]string{}, sizes: map[string][2]uint16{}}
	for _, id := range live {
		p.live[id] = true
	}
	return p
}

func (p *termPTY) HasTerminal(id string) bool { p.mu.Lock(); defer p.mu.Unlock(); return p.live[id] }
func (p *termPTY) setLive(id string)          { p.mu.Lock(); defer p.mu.Unlock(); p.live[id] = true }
func (p *termPTY) typed(id string) string     { p.mu.Lock(); defer p.mu.Unlock(); return p.input[id] }
func (p *termPTY) SendInput(id string, data []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.input[id] += string(data)
	return nil
}
func (p *termPTY) ResizeSession(id string, rows, cols uint16) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sizes[id] = [2]uint16{rows, cols}
	return nil
}
func (p *termPTY) AttachSessionWithSnapshot(context.Context, string, io.Writer, int64, func(agentsessions.AttachSnapshot) error) error {
	return errors.New("termPTY: no output")
}

// terminalReq is one /terminal-attachments request, with an optional writer lease.
func (c client) terminalReq(method, path, body, lease string) (int, map[string]any) {
	c.t.Helper()
	req, err := http.NewRequest(method, c.base+path, strings.NewReader(body))
	if err != nil {
		c.t.Fatal(err)
	}
	if lease != "" {
		req.Header.Set("X-Tether-Writer-Lease", lease)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	out := map[string]any{}
	_ = json.NewDecoder(bytes.NewReader(raw)).Decode(&out)
	return resp.StatusCode, out
}

func errMessage(body map[string]any) string {
	e, _ := body["error"].(map[string]any)
	m, _ := e["message"].(string)
	return m
}

// With both flags on, the daemon's wiring: one chat host for the chat wire and /terminal-attachments. A chat controller
// and a terminal controller contend for the one input lease, a takeover by either refuses the other's input per its
// wire (terminal 409, chat 403 lease_required), and after a relaunch the old terminal attachment is refused as stale.
func TestTerminalAttachmentsShareTheChatLease(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := tstore.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sessions := newFakeSessions(t)
	sessions.set("s1", RuntimeClaudeStdio)
	(&env{st: st}).session(t, "s1", "seat", "")

	host, err := OpenHost(dir)
	if err != nil {
		t.Fatal(err)
	}
	w, err := Open(ctx, Config{StateDir: dir, Host: host, Store: st, Sessions: sessions})
	if err != nil {
		t.Fatal(err)
	}
	pty := newTermPTY("s1")
	th, err := NewTerminalHost(TerminalConfig{Host: host, Store: st, Sessions: pty})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(api.NewHandler(api.Deps{ChatWire: w.Handler(), TerminalHost: th}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { srv.CloseClientConnections(); _ = w.Close(context.Background()) })
	c := client{t: t, base: srv.URL}

	// The chat side: the seat's conversation, a controller attachment, and the lease.
	status, conv := c.do(http.MethodPost, "/chat/v1/agents/seat/conversation", nil)
	if status != http.StatusOK {
		t.Fatalf("ensure = %d %v", status, conv)
	}
	convID := conv["conversation_id"].(string)
	status, a := c.do(http.MethodPost, "/chat/v1/conversations/"+convID+"/attachments", map[string]string{"role": "controller", "stream": "deltas"})
	if status != http.StatusCreated {
		t.Fatalf("chat attach = %d %v", status, a)
	}
	chatCtl := a["attachment_id"].(string)
	if status, body := c.do(http.MethodPost, "/chat/v1/conversations/"+convID+"/lease", map[string]string{"attachment": chatCtl}); status != http.StatusOK {
		t.Fatalf("chat lease = %d %v", status, body)
	}

	// The terminal side, on the same conversation: it resolves to the live PTY at the same generation.
	status, opened := c.terminalReq(http.MethodPost, "/terminal-attachments", `{"target":"`+convID+`","role":"controller"}`, "")
	if status != http.StatusCreated || opened["session_id"] != "s1" || opened["generation"] != float64(1) || opened["conversation_id"] != convID {
		t.Fatalf("terminal open = %d %v", status, opened)
	}
	termID := opened["id"].(string)

	// The chat controller holds the one lease: the terminal cannot acquire it without a takeover.
	if status, body := c.terminalReq(http.MethodPost, "/terminal-attachments/"+termID+"/lease", `{"takeover":false}`, ""); status != http.StatusConflict {
		t.Fatalf("terminal acquire while chat holds the lease = %d %v", status, body)
	}
	status, claimed := c.terminalReq(http.MethodPost, "/terminal-attachments/"+termID+"/lease", `{"takeover":true}`, "")
	if status != http.StatusOK {
		t.Fatalf("terminal takeover = %d %v", status, claimed)
	}
	termLease := claimed["lease"].(string)
	if status, body := c.terminalReq(http.MethodPost, "/terminal-attachments/"+termID+"/input", "ls\r", termLease); status != http.StatusNoContent {
		t.Fatalf("terminal input = %d %v", status, body)
	}
	if status, body := c.terminalReq(http.MethodPost, "/terminal-attachments/"+termID+"/resize", `{"rows":40,"cols":120}`, termLease); status != http.StatusNoContent {
		t.Fatalf("terminal resize = %d %v", status, body)
	}
	if got := pty.typed("s1"); got != "ls\r" {
		t.Fatalf("PTY input = %q", got)
	}

	// The terminal's takeover refuses the chat controller's input: 403 lease_required on the chat wire.
	status, body := c.do(http.MethodPost, "/chat/v1/conversations/"+convID+"/turns", map[string]string{"attachment": chatCtl, "text": "hello", "request_id": "r1"})
	if status != http.StatusForbidden || body["code"] != "lease_required" {
		t.Fatalf("chat submit after terminal takeover = %d %v", status, body)
	}

	// A chat takeover refuses the terminal's input: 409 on /terminal-attachments, nothing reaches the PTY.
	if status, body := c.do(http.MethodPost, "/chat/v1/conversations/"+convID+"/lease/takeover", map[string]string{"attachment": chatCtl}); status != http.StatusOK {
		t.Fatalf("chat takeover = %d %v", status, body)
	}
	if status, body := c.terminalReq(http.MethodPost, "/terminal-attachments/"+termID+"/input", "rm\r", termLease); status != http.StatusConflict {
		t.Fatalf("terminal input after chat takeover = %d %v", status, body)
	}
	if got := pty.typed("s1"); got != "ls\r" {
		t.Fatalf("PTY input after chat takeover = %q", got)
	}

	// Back to the terminal, then a relaunch: the conversation moves to s1r at generation 2.
	status, claimed = c.terminalReq(http.MethodPost, "/terminal-attachments/"+termID+"/lease", `{"takeover":true}`, "")
	if status != http.StatusOK {
		t.Fatalf("terminal takeover again = %d %v", status, claimed)
	}
	termLease = claimed["lease"].(string)
	sessions.set("s1r", RuntimeClaudeStdio)
	pty.setLive("s1r")
	(&env{st: st}).session(t, "s1r", "seat", "s1")

	// The old generation's terminal attachment is stale (409), for input and for its lease, and nothing is typed.
	status, body = c.terminalReq(http.MethodPost, "/terminal-attachments/"+termID+"/input", "old\r", termLease)
	if status != http.StatusConflict || !strings.Contains(errMessage(body), "generation") {
		t.Fatalf("stale terminal input = %d %v", status, body)
	}
	if status, body := c.terminalReq(http.MethodPost, "/terminal-attachments/"+termID+"/lease", `{"takeover":true}`, ""); status != http.StatusConflict {
		t.Fatalf("stale terminal lease = %d %v", status, body)
	}
	if pty.typed("s1") != "ls\r" || pty.typed("s1r") != "" {
		t.Fatalf("a stale attachment reached a PTY: s1 %q s1r %q", pty.typed("s1"), pty.typed("s1r"))
	}
	// Targeting the superseded session explicitly is stale too; the conversation resolves to the new session.
	if status, body := c.terminalReq(http.MethodPost, "/terminal-attachments", `{"target":"s1","role":"controller"}`, ""); status != http.StatusConflict {
		t.Fatalf("open superseded session = %d %v", status, body)
	}
	status, opened = c.terminalReq(http.MethodPost, "/terminal-attachments", `{"target":"`+convID+`","role":"controller"}`, "")
	if status != http.StatusCreated || opened["session_id"] != "s1r" || opened["generation"] != float64(2) {
		t.Fatalf("terminal open after relaunch = %d %v", status, opened)
	}
	termID = opened["id"].(string)

	// The new generation still has one lease: the chat side, at generation 2 too, contends with the new terminal.
	status, claimed = c.terminalReq(http.MethodPost, "/terminal-attachments/"+termID+"/lease", `{"takeover":false}`, "")
	if status != http.StatusOK {
		t.Fatalf("terminal lease at generation 2 = %d %v", status, claimed)
	}
	if status, body := c.terminalReq(http.MethodPost, "/terminal-attachments/"+termID+"/input", "new\r", claimed["lease"].(string)); status != http.StatusNoContent {
		t.Fatalf("terminal input at generation 2 = %d %v", status, body)
	}
	if got := pty.typed("s1r"); got != "new\r" {
		t.Fatalf("PTY s1r input = %q", got)
	}
	status, a = c.do(http.MethodPost, "/chat/v1/conversations/"+convID+"/attachments", map[string]string{"role": "controller", "stream": "deltas"})
	if status != http.StatusCreated {
		t.Fatalf("chat attach at generation 2 = %d %v", status, a)
	}
	if status, body := c.do(http.MethodPost, "/chat/v1/conversations/"+convID+"/lease", map[string]string{"attachment": a["attachment_id"].(string)}); status != http.StatusConflict {
		t.Fatalf("chat lease while the terminal holds it = %d %v", status, body)
	}
}

// Only a live PTY on this daemon is a terminal; a target that is missing or not the caller's is not found.
func TestTerminalTargets(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.session(t, "s1", "seat", "")
	pty := newTermPTY()
	targets := &TerminalTargets{Resolver: conversation.Resolver{Store: e.st}, Sessions: pty, Authorize: func(context.Context, string) error { return nil }}

	tc, err := e.resolver.BySeat(ctx, "seat")
	if err != nil {
		t.Fatal(err)
	}
	b, err := targets.Resolve(ctx, "s1")
	if err != nil || b.Terminal || b.SessionID != "s1" || b.ConversationID != tc.ConversationID {
		t.Fatalf("no PTY: %+v %v", b, err)
	}
	pty.setLive("s1")
	b, err = targets.Resolve(ctx, tc.ConversationID)
	want := HostBinding(tc.Owner, "seat", "seat", tc)
	if err != nil || !b.Terminal || b.OwnerID != string(want.OwnerID) || b.OperativeID != "seat" || b.SeatID != "seat" ||
		b.AttemptID != tc.AttemptID || b.Generation != tc.Generation {
		t.Fatalf("live PTY: %+v %v; want the chat host binding %+v", b, err, want)
	}
	if _, err := targets.Resolve(ctx, "missing"); !errors.Is(err, terminalattach.ErrNotFound) {
		t.Fatalf("missing target: %v", err)
	}
	targets.Authorize = func(context.Context, string) error { return ErrForbidden }
	if _, err := targets.Resolve(ctx, "s1"); !errors.Is(err, terminalattach.ErrNotFound) {
		t.Fatalf("refused caller: %v", err)
	}
}

// daemon.terminal_attachments on alone: no chat wire, and the chat host learns the conversation from the terminal side.
func TestTerminalAttachmentsWithoutChatWire(t *testing.T) {
	e := newEnv(t)
	e.session(t, "s1", "seat", "")
	pty := newTermPTY("s1")
	th, err := NewTerminalHost(TerminalConfig{Host: e.host, Store: e.st, Sessions: pty})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(api.NewHandler(api.Deps{TerminalHost: th}))
	t.Cleanup(srv.Close)
	c := client{t: t, base: srv.URL}
	if status, _ := c.do(http.MethodPost, "/chat/v1/agents/seat/conversation", nil); status != http.StatusNotFound {
		t.Fatalf("chat wire with only terminal attachments on = %d", status)
	}
	status, opened := c.terminalReq(http.MethodPost, "/terminal-attachments", `{"target":"s1","role":"controller"}`, "")
	if status != http.StatusCreated {
		t.Fatalf("open = %d %v", status, opened)
	}
	id := opened["id"].(string)
	status, claimed := c.terminalReq(http.MethodPost, "/terminal-attachments/"+id+"/lease", `{}`, "")
	if status != http.StatusOK {
		t.Fatalf("lease = %d %v", status, claimed)
	}
	if status, body := c.terminalReq(http.MethodPost, "/terminal-attachments/"+id+"/input", "x", claimed["lease"].(string)); status != http.StatusNoContent {
		t.Fatalf("input = %d %v", status, body)
	}
	if pty.typed("s1") != "x" {
		t.Fatalf("PTY input = %q", pty.typed("s1"))
	}
	// A relaunch first seen by the terminal side (the chat host steps from generation 1 itself).
	pty.setLive("s1r")
	e.relaunch(t, "seat")
	if status, _ := c.terminalReq(http.MethodPost, "/terminal-attachments/"+id+"/input", "y", claimed["lease"].(string)); status != http.StatusConflict {
		t.Fatalf("stale input = %d", status)
	}
	if status, opened := c.terminalReq(http.MethodPost, "/terminal-attachments", `{"target":"s1r"}`, ""); status != http.StatusCreated || opened["generation"] != float64(2) {
		t.Fatalf("open after relaunch = %d %v", status, opened)
	}
}
