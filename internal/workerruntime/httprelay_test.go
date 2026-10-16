package workerruntime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hollis-labs/libs/security/secrets/tokenfile"
)

const (
	tokenOne = "tok-ONE-aaaaaaaaaaaaaaaaaaaaaaaa"
	tokenTwo = "tok-TWO-bbbbbbbbbbbbbbbbbbbbbbbb"
)

// fakeHub is a streamable-HTTP MCP endpoint reduced to what the relay depends on.
type fakeHub struct {
	mu       sync.Mutex
	current  string // the one token the hub accepts
	seen     []string
	methods  []string
	getCount atomic.Int32
	deletes  atomic.Int32
	// refuse, when set, answers every tools/list with this status and code.
	refuseStatus int
	refuseCode   string
}

func (h *fakeHub) accepts(token string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return token == h.current
}

func (h *fakeHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	h.mu.Lock()
	h.seen = append(h.seen, token)
	h.mu.Unlock()
	if !h.accepts(token) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"code":"unauthorized"}}`)
		return
	}
	switch r.Method {
	case http.MethodDelete:
		h.deletes.Add(1)
		w.WriteHeader(http.StatusNoContent)
	case http.MethodGet:
		if r.Header.Get("Mcp-Session-Id") != "sess-1" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if h.getCount.Add(1) > 1 {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/tools/list_changed\"}\n\n")
	case http.MethodPost:
		body, _ := io.ReadAll(r.Body)
		var m struct {
			Method string          `json:"method"`
			ID     json.RawMessage `json:"id"`
		}
		_ = json.Unmarshal(body, &m)
		h.mu.Lock()
		h.methods = append(h.methods, m.Method)
		h.mu.Unlock()
		switch m.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "sess-1")
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2025-03-26","capabilities":{},"serverInfo":{"name":"hub","version":"1"}}}`, m.ID)
		case "tools/list":
			if r.Header.Get("Mcp-Session-Id") != "sess-1" || r.Header.Get("MCP-Protocol-Version") != "2025-03-26" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if h.refuseStatus != 0 {
				w.WriteHeader(h.refuseStatus)
				_, _ = fmt.Fprintf(w, `{"error":{"code":%q}}`, h.refuseCode)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":{\"tools\":[{\"name\":\"tether_health\"}]}}\n\n", m.ID)
		default:
			w.WriteHeader(http.StatusAccepted)
		}
	}
}

// relayRig runs an HTTPRelay against a fakeHub, exposing the agent side of the stdio pipes.
type relayRig struct {
	t      *testing.T
	hub    *fakeHub
	srv    *httptest.Server
	file   string
	stderr *bytes.Buffer
	in     *io.PipeWriter
	out    *bufio.Scanner
	done   chan error
}

func newRelayRig(t *testing.T, hub *fakeHub, transport http.RoundTripper) *relayRig {
	t.Helper()
	hub.current = tokenOne
	srv := httptest.NewServer(hub)
	t.Cleanup(srv.Close)
	file := filepath.Join(t.TempDir(), "token")
	if err := tokenfile.Write(file, tokenOne); err != nil {
		t.Fatal(err)
	}
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	stderr := &bytes.Buffer{}
	client := srv.Client()
	if transport != nil {
		client = &http.Client{Transport: transport}
	}
	r := &HTTPRelay{URL: srv.URL, TokenFile: file, Client: client, Stderr: stderr}
	done := make(chan error, 1)
	go func() {
		err := r.Run(context.Background(), inR, outW)
		_ = outW.Close()
		done <- err
	}()
	return &relayRig{t: t, hub: hub, srv: srv, file: file, stderr: stderr, in: inW, out: bufio.NewScanner(outR), done: done}
}

func (g *relayRig) send(line string) {
	g.t.Helper()
	if _, err := io.WriteString(g.in, line+"\n"); err != nil {
		g.t.Fatal(err)
	}
}

func (g *relayRig) recv() map[string]any {
	g.t.Helper()
	got := make(chan bool, 1)
	go func() { got <- g.out.Scan() }()
	select {
	case ok := <-got:
		if !ok {
			g.t.Fatalf("relay output ended: %v", g.out.Err())
		}
	case <-time.After(5 * time.Second):
		g.t.Fatal("no message from the relay")
	}
	var m map[string]any
	if err := json.Unmarshal(g.out.Bytes(), &m); err != nil {
		g.t.Fatalf("relay wrote %q: %v", g.out.Text(), err)
	}
	return m
}

func (g *relayRig) finish() {
	g.t.Helper()
	_ = g.in.Close()
	select {
	case err := <-g.done:
		if err != nil {
			g.t.Fatalf("relay: %v", err)
		}
	case <-time.After(5 * time.Second):
		g.t.Fatal("relay did not stop after stdin closed")
	}
}

func (g *relayRig) handshake() {
	g.t.Helper()
	g.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`)
	if m := g.recv(); m["id"] != float64(1) || m["result"] == nil {
		g.t.Fatalf("initialize reply = %v", m)
	}
	g.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
}

func TestHTTPRelayRoundTripSessionStreamAndClose(t *testing.T) {
	hub := &fakeHub{}
	g := newRelayRig(t, hub, nil)
	g.handshake()
	// The standalone server-to-agent stream opened after initialize delivers a notification.
	if m := g.recv(); m["method"] != "notifications/tools/list_changed" {
		t.Fatalf("server notification = %v", m)
	}
	// A request answered as an SSE stream, with the session and negotiated protocol on the wire.
	g.send(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	m := g.recv()
	if m["id"] != float64(2) || m["result"] == nil {
		t.Fatalf("tools/list reply = %v", m)
	}
	g.finish()
	if hub.deletes.Load() != 1 {
		t.Fatalf("hub session was not closed on agent exit (DELETEs = %d)", hub.deletes.Load())
	}
}

// Issuer tokens are rewritten in place before they expire; the relay must send the file's current content on
// every request, never the value it first saw.
func TestHTTPRelayRereadsTokenPerRequest(t *testing.T) {
	hub := &fakeHub{}
	g := newRelayRig(t, hub, nil)
	g.handshake()
	_ = g.recv() // standalone-stream notification
	hub.mu.Lock()
	hub.current = tokenTwo
	hub.mu.Unlock()
	if err := tokenfile.Write(g.file, tokenTwo); err != nil {
		t.Fatal(err)
	}
	g.send(`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`)
	if m := g.recv(); m["id"] != float64(3) || m["result"] == nil {
		t.Fatalf("request after rotation = %v", m)
	}
	g.finish()
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if last := hub.seen[len(hub.seen)-1]; last != tokenTwo && last != tokenOne {
		t.Fatalf("unexpected final token %q", last)
	}
	sawTwo := false
	for _, tok := range hub.seen {
		sawTwo = sawTwo || tok == tokenTwo
	}
	if !sawTwo {
		t.Fatal("the rotated token was never sent")
	}
}

// rotateOnFirstPost rewrites the token file (and the hub's accepted token) just after the relay has read the
// old value and before the request reaches the hub: the 401 a real rotation race produces.
type rotateOnFirstPost struct {
	base   http.RoundTripper
	rotate func()
	once   sync.Once
}

func (r *rotateOnFirstPost) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodPost && strings.Contains(req.Header.Get("Authorization"), tokenOne) {
		r.once.Do(r.rotate)
	}
	return r.base.RoundTrip(req)
}

func TestHTTPRelayRetriesOnceAfterRotationAndNeverLogsTheToken(t *testing.T) {
	hub := &fakeHub{}
	var g *relayRig
	rt := &rotateOnFirstPost{base: http.DefaultTransport}
	g = newRelayRig(t, hub, rt)
	rt.rotate = func() {
		hub.mu.Lock()
		hub.current = tokenTwo
		hub.mu.Unlock()
		if err := tokenfile.Write(g.file, tokenTwo); err != nil {
			t.Error(err)
		}
	}
	g.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`)
	if m := g.recv(); m["id"] != float64(1) || m["result"] == nil {
		t.Fatalf("initialize after a rotation race = %v", m)
	}
	_ = g.recv() // the standalone-stream notification; the pipe is unbuffered, so it must be consumed
	g.finish()
	hub.mu.Lock()
	seen := append([]string(nil), hub.seen...)
	hub.mu.Unlock()
	if len(seen) < 2 || seen[0] != tokenOne || seen[1] != tokenTwo {
		t.Fatalf("hub saw tokens %v, want one then two", seen)
	}
	for _, secret := range []string{tokenOne, tokenTwo} {
		if strings.Contains(g.stderr.String(), secret) {
			t.Fatalf("a token was written to the relay's diagnostics: %q", g.stderr.String())
		}
	}
}

// A refusal is reported to the agent as a JSON-RPC error carrying only the status and the hub's code.
func TestHTTPRelayReportsRefusalWithoutSecrets(t *testing.T) {
	hub := &fakeHub{refuseStatus: http.StatusForbidden, refuseCode: "mcp_policy_refused"}
	g := newRelayRig(t, hub, nil)
	g.handshake()
	_ = g.recv()
	g.send(`{"jsonrpc":"2.0","id":9,"method":"tools/list"}`)
	m := g.recv()
	errObj, _ := m["error"].(map[string]any)
	msg, _ := errObj["message"].(string)
	if m["id"] != float64(9) || !strings.Contains(msg, "403") || !strings.Contains(msg, "mcp_policy_refused") {
		t.Fatalf("refusal = %v", m)
	}
	g.finish()
	for _, secret := range []string{tokenOne, tokenTwo} {
		if strings.Contains(msg, secret) || strings.Contains(g.stderr.String(), secret) {
			t.Fatal("a token leaked into an error or diagnostic")
		}
	}
}

// A 401 that persists (the file still holds the same token) is not retried forever.
func TestHTTPRelayDoesNotLoopOnPersistent401(t *testing.T) {
	hub := &fakeHub{}
	g := newRelayRig(t, hub, nil)
	hub.mu.Lock()
	hub.current = "some-other-token"
	hub.mu.Unlock()
	g.send(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`)
	m := g.recv()
	if m["error"] == nil || m["id"] != float64(1) {
		t.Fatalf("persistent 401 reply = %v", m)
	}
	g.finish()
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if len(hub.seen) != 1 {
		t.Fatalf("hub saw %d requests, want exactly 1 (no retry with an unchanged token)", len(hub.seen))
	}
}

func TestHTTPRelayRefusesUnsafeEndpointsAndTokenFiles(t *testing.T) {
	file := filepath.Join(t.TempDir(), "token")
	if err := tokenfile.Write(file, tokenOne); err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{"http://hub.example.com/mcp", "ftp://hub/mcp", "https://user:pw@hub.example.com/mcp", "", "https:///mcp"} {
		r := &HTTPRelay{URL: u, TokenFile: file}
		if err := r.Run(context.Background(), strings.NewReader(""), io.Discard); err == nil {
			t.Errorf("url %q accepted", u)
		}
	}
	for _, u := range []string{"https://hub.example.com/mcp", "http://127.0.0.1:1/mcp", "http://localhost/mcp", "http://[::1]:8/mcp"} {
		r := &HTTPRelay{URL: u, TokenFile: file}
		if err := r.validate(); err != nil {
			t.Errorf("url %q refused: %v", u, err)
		}
	}
	loose := filepath.Join(t.TempDir(), "loose")
	if err := os.WriteFile(loose, []byte(tokenOne+"\n"), 0o644); err != nil { //nolint:gosec // G306: the test needs a too-open file
		t.Fatal(err)
	}
	err := (&HTTPRelay{URL: "https://hub.example.com/mcp", TokenFile: loose}).Run(context.Background(), strings.NewReader(""), io.Discard)
	if err == nil || strings.Contains(err.Error(), tokenOne) {
		t.Fatalf("loose-mode token file: err = %v", err)
	}
}

// A redirect is never followed: Go would keep the Authorization header on a same-hostname redirect, including an
// https to http downgrade. The relay fails the request and the redirect target never sees a credential, even when the
// caller injects a client whose own policy would follow it.
func TestHTTPRelayNeverFollowsRedirectsOrLeaksTheBearer(t *testing.T) {
	var targetHits, targetAuth atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		if r.Header.Get("Authorization") != "" {
			targetAuth.Add(1)
		}
	}))
	t.Cleanup(target.Close)
	hub := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/mcp", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(hub.Close)
	file := filepath.Join(t.TempDir(), "token")
	if err := tokenfile.Write(file, tokenOne); err != nil {
		t.Fatal(err)
	}
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	stderr := &bytes.Buffer{}
	// hub.Client() trusts the test certificate and has Go's default redirect policy.
	relay := &HTTPRelay{URL: hub.URL, TokenFile: file, Client: hub.Client(), Stderr: stderr}
	done := make(chan error, 1)
	go func() { err := relay.Run(context.Background(), inR, outW); _ = outW.Close(); done <- err }()
	reply := bufio.NewScanner(outR)
	if _, err := io.WriteString(inW, `{"jsonrpc":"2.0","id":1,"method":"initialize"}`+"\n"); err != nil {
		t.Fatal(err)
	}
	scanned := make(chan bool, 1)
	go func() { scanned <- reply.Scan() }()
	select {
	case ok := <-scanned:
		if !ok {
			t.Fatal("no reply")
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("no reply in 5s (a followed redirect leaves the request unanswered); target hits %d, with credential %d", targetHits.Load(), targetAuth.Load())
	}
	var m map[string]any
	if err := json.Unmarshal(reply.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	errObj, _ := m["error"].(map[string]any)
	msg, _ := errObj["message"].(string)
	if m["id"] != float64(1) || !strings.Contains(msg, "307") || !strings.Contains(msg, "redirect_refused") {
		t.Fatalf("redirect reply = %v", m)
	}
	_ = inW.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if targetHits.Load() != 0 || targetAuth.Load() != 0 {
		t.Fatalf("the redirect target was reached (%d hits, %d with a credential)", targetHits.Load(), targetAuth.Load())
	}
	if strings.Contains(stderr.String(), tokenOne) || strings.Contains(msg, tokenOne) {
		t.Fatal("the token leaked into a diagnostic")
	}
}

// The default client carries the same policy.
func TestHTTPRelayDefaultClientRefusesRedirects(t *testing.T) {
	c := (&HTTPRelay{}).client()
	if c.CheckRedirect == nil || !errors.Is(c.CheckRedirect(nil, nil), http.ErrUseLastResponse) {
		t.Fatal("the default client would follow redirects")
	}
	injected := (&HTTPRelay{Client: &http.Client{}}).client()
	if injected.CheckRedirect == nil || !errors.Is(injected.CheckRedirect(nil, nil), http.ErrUseLastResponse) {
		t.Fatal("an injected client would follow redirects")
	}
}
