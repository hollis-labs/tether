package workerruntime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hollis-labs/libs/harness/core/adapters/runtimeevents/ingest"

	"github.com/hollis-labs/tether/internal/identity"
)

type hubStub struct {
	srv    *httptest.Server
	mu     sync.Mutex
	calls  int
	auth   string
	path   string
	method string
	proto  string
	body   []byte
	status int
	reply  string
	retry  string
}

func newHub(t *testing.T) *hubStub {
	t.Helper()
	h := &hubStub{status: 200, reply: `{"results":[],"accepted":0,"duplicate":0,"rejected":0}`}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.calls++
		h.auth, h.path, h.method, h.proto, h.body = r.Header.Get("Authorization"), r.URL.Path, r.Method, r.Header.Get("Tether-Protocol"), b
		status, reply, retry := h.status, h.reply, h.retry
		h.mu.Unlock()
		if retry != "" {
			w.Header().Set("Retry-After", retry)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *hubStub) snapshot() (calls int, auth, path string, body []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls, h.auth, h.path, h.body
}

func tokenFile(t *testing.T) (path, token string) {
	t.Helper()
	tok, err := identity.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	path = filepath.Join(t.TempDir(), "device.token")
	if err := os.WriteFile(path, []byte(tok+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, tok
}

func relayFor(t *testing.T, hub *hubStub, token string, _ *bytes.Buffer) *EventsRelay {
	t.Helper()
	// port stands in for the bound port when a test drives Handler directly;
	// Serve replaces it with the real one.
	return &EventsRelay{Listen: "127.0.0.1:0", Hub: hub.srv.URL, TokenFile: token, port: "7845"}
}

const oneEvent = `{"schema_version":"1","id":"evt_00000000000000000000000000000001","kind":"alert.raised","app":"cerberus","sequence":1,"time":"2026-10-20T01:00:00Z","source":{"channel":"t"},"subject":"msg://alert/cerberus/1"}`

func post(h http.Handler, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7845"+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestRelayForwardsVerbatimWithTheWorkersCredential(t *testing.T) {
	hub := newHub(t)
	tf, tok := tokenFile(t)
	r := relayFor(t, hub, tf, nil)
	// A caller-supplied Authorization header is ignored, never forwarded.
	w := post(r.Handler(), "/events", oneEvent, map[string]string{"Authorization": "Bearer attacker"})
	if w.Code != 200 || w.Body.String() != hub.reply {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	calls, auth, path, body := hub.snapshot()
	if calls != 1 || path != "/events" || auth != "Bearer "+tok || string(body) != oneEvent {
		t.Fatalf("calls=%d path=%q auth-ok=%v body-ok=%v", calls, path, auth == "Bearer "+tok, string(body) == oneEvent)
	}
	if hub.proto != "1" || hub.method != http.MethodPost {
		t.Errorf("proto=%q method=%q", hub.proto, hub.method)
	}
}

func TestRelayPassesHubStatusRetryAfterAndBodyThrough(t *testing.T) {
	for _, c := range []struct {
		status int
		retry  string
		body   string
	}{
		{429, "3", `{"error":{"code":"rate_limited"}}`},
		{503, "1", `{"error":{"code":"events_unavailable"}}`},
		{403, "", `{"error":{"code":"app_mismatch"}}`},
		{401, "", `{"error":{"code":"unauthorized"}}`},
		{405, "", `{"error":{"code":"method_not_allowed"}}`},
	} {
		hub := newHub(t)
		hub.status, hub.retry, hub.reply = c.status, c.retry, c.body
		tf, _ := tokenFile(t)
		w := post(relayFor(t, hub, tf, nil).Handler(), "/events", oneEvent, nil)
		if w.Code != c.status || w.Body.String() != c.body || w.Header().Get("Retry-After") != c.retry {
			t.Errorf("%d: got %d %q retry=%q", c.status, w.Code, w.Body, w.Header().Get("Retry-After"))
		}
	}
}

func TestRelayRefusesWithoutACredentialAndDoesNotCallTheHub(t *testing.T) {
	hub := newHub(t)
	r := relayFor(t, hub, filepath.Join(t.TempDir(), "missing.token"), nil)
	w := post(r.Handler(), "/events", oneEvent, nil)
	if w.Code != 503 || w.Header().Get("Retry-After") == "" || !strings.Contains(w.Body.String(), "relay_no_credential") {
		t.Fatalf("%d %s retry=%q", w.Code, w.Body, w.Header().Get("Retry-After"))
	}
	if calls, _, _, _ := hub.snapshot(); calls != 0 {
		t.Fatal("hub was called without a credential")
	}
}

func TestRelayAppliesTheContractLimitsItselfAndNeverCallsTheHubForBadRequests(t *testing.T) {
	hub := newHub(t)
	tf, _ := tokenFile(t)
	r := relayFor(t, hub, tf, nil).Handler()
	many := `{"events":[` + strings.Repeat(`{},`, ingest.MaxBatchEvents) + `{}]}`
	big := `{"id":"` + strings.Repeat("a", ingest.MaxRequestBytes) + `"}`
	for _, c := range []struct {
		name, body string
		want       int
	}{
		{"not json", "nope", 400}, {"empty batch", `{"events":[]}`, 400}, {"array body", `[{}]`, 400},
		{"too many", many, 413}, {"too big", big, 413},
	} {
		if w := post(r, "/events", c.body, nil); w.Code != c.want {
			t.Errorf("%s: %d %s", c.name, w.Code, w.Body)
		}
	}
	if calls, _, _, _ := hub.snapshot(); calls != 0 {
		t.Fatalf("hub called %d times for bad requests", calls)
	}
}

func TestRelaySurface(t *testing.T) {
	hub := newHub(t)
	tf, _ := tokenFile(t)
	h := relayFor(t, hub, tf, nil).Handler()
	if w := post(h, "/other", oneEvent, nil); w.Code != 404 {
		t.Errorf("other path: %d", w.Code)
	}
	req := httptest.NewRequest(http.MethodGet, "/events", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 405 {
		t.Errorf("GET /events: %d", w.Code)
	}
	req = httptest.NewRequest(http.MethodGet, "/health", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "ok") {
		t.Errorf("health: %d %s", w.Code, w.Body)
	}
}

func TestRelayHubDownIsRetryable(t *testing.T) {
	hub := newHub(t)
	tf, _ := tokenFile(t)
	r := relayFor(t, hub, tf, nil)
	hub.srv.Close()
	w := post(r.Handler(), "/events", oneEvent, nil)
	if w.Code != 502 || w.Header().Get("Retry-After") == "" {
		t.Fatalf("%d retry=%q", w.Code, w.Header().Get("Retry-After"))
	}
}

func TestRelayNeverLogsTokensOrBodies(t *testing.T) {
	hub := newHub(t)
	tf, tok := tokenFile(t)
	var logs bytes.Buffer
	r := &EventsRelay{Listen: "127.0.0.1:7845", Hub: hub.srv.URL, TokenFile: tf, Logf: func(f string, a ...any) {
		logs.WriteString(fmt.Sprintf(f, a...) + "\n")
	}}
	post(r.Handler(), "/events", oneEvent, nil)
	hub.mu.Lock()
	hub.status = 500
	hub.mu.Unlock()
	post(r.Handler(), "/events", oneEvent, nil)
	if !strings.Contains(logs.String(), "forwarded 1 event(s), hub status 200") {
		t.Fatalf("expected a diagnostic line, got %q", logs.String())
	}
	for _, leak := range []string{tok, "alert.raised", "cerberus", "evt_"} {
		if strings.Contains(logs.String(), leak) {
			t.Fatalf("log leaks %q: %s", leak, logs.String())
		}
	}
}

func TestRelayConfigValidation(t *testing.T) {
	good := &EventsRelay{Listen: "127.0.0.1:0", Hub: "https://hub.example"}
	if err := good.validate(); err != nil {
		t.Fatal(err)
	}
	for _, c := range [][2]string{
		{"0.0.0.0:7845", "https://hub.example"},
		{":7845", "https://hub.example"},
		{"192.168.1.5:7845", "https://hub.example"},
		{"7845", "https://hub.example"},
		{"127.0.0.1:7845", "http://hub.example"},
		{"127.0.0.1:7845", "https://user:pw@hub.example"},
		{"127.0.0.1:7845", ""},
	} {
		r := &EventsRelay{Listen: c[0], Hub: c[1]}
		if err := r.validate(); !errors.Is(err, ErrEventsRelayConfig) {
			t.Errorf("listen=%q hub=%q accepted: %v", c[0], c[1], err)
		}
	}
	if err := (&EventsRelay{Listen: "127.0.0.1:0", Hub: "http://127.0.0.1:8000"}).validate(); err != nil {
		t.Errorf("loopback http hub: %v", err)
	}
}

func TestRelayServesOnLoopbackAndStops(t *testing.T) {
	hub := newHub(t)
	tf, tok := tokenFile(t)
	r := relayFor(t, hub, tf, nil)
	ctx, cancel := context.WithCancel(context.Background())
	addr := make(chan string, 1)
	done := make(chan error, 1)
	go func() { done <- r.Serve(ctx, func(a net.Addr) { addr <- a.String() }) }()
	var a string
	select {
	case a = <-addr:
	case <-time.After(5 * time.Second):
		t.Fatal("relay did not start")
	}
	res, err := http.Post("http://"+a+"/events", "application/json", strings.NewReader(oneEvent))
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if _, auth, _, _ := hub.snapshot(); res.StatusCode != 200 || auth != "Bearer "+tok {
		t.Fatalf("status %d", res.StatusCode)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("relay did not stop")
	}
}

// A web page can reach a loopback port with a "simple" POST (text/plain, no
// preflight) or through a rebound DNS name; neither may use the device credential.
func TestRelayRefusesWhatAWebPageCanSend(t *testing.T) {
	hub := newHub(t)
	tf, _ := tokenFile(t)
	r := relayFor(t, hub, tf, nil)
	cases := []struct {
		name string
		host string
		hdr  map[string]string
		want int
	}{
		{"text/plain simple POST", "127.0.0.1:7845", map[string]string{"Content-Type": "text/plain"}, http.StatusUnsupportedMediaType},
		{"form POST", "127.0.0.1:7845", map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, http.StatusUnsupportedMediaType},
		{"cross-origin json", "127.0.0.1:7845", map[string]string{"Origin": "https://evil.example"}, http.StatusForbidden},
		{"rebound DNS name", "evil.example:7845", nil, http.StatusMisdirectedRequest},
		{"loopback name on another port", "127.0.0.1:9999", nil, http.StatusMisdirectedRequest},
		{"loopback name without a port", "127.0.0.1", nil, http.StatusMisdirectedRequest},
		{"json to localhost", "localhost:7845", nil, http.StatusOK},
		{"json to ipv6 loopback", "[::1]:7845", nil, http.StatusOK},
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodPost, "http://"+c.host+"/events", strings.NewReader(oneEvent))
		req.Header.Set("Content-Type", "application/json")
		for k, v := range c.hdr {
			req.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		r.Handler().ServeHTTP(w, req)
		if w.Code != c.want {
			t.Errorf("%s: %d, want %d", c.name, w.Code, c.want)
		}
		for k := range w.Header() {
			if strings.HasPrefix(strings.ToLower(k), "access-control-") {
				t.Errorf("%s: CORS header %s set", c.name, k)
			}
		}
	}
	if calls, _, _, _ := hub.snapshot(); calls != 2 {
		t.Errorf("hub was called %d times; only the two same-host JSON requests may reach it", calls)
	}
}

// Ten sequential requests ride one pooled connection, not ten.
func TestRelayReusesOneHubConnection(t *testing.T) {
	var conns atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[]}`)
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conns.Add(1)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)
	tf, _ := tokenFile(t)
	r := &EventsRelay{Listen: "127.0.0.1:0", Hub: srv.URL, TokenFile: tf, port: "7845"}
	for i := 0; i < 10; i++ {
		if w := post(r.Handler(), "/events", oneEvent, nil); w.Code != 200 {
			t.Fatalf("request %d: %d", i, w.Code)
		}
	}
	if n := conns.Load(); n != 1 {
		t.Fatalf("%d hub connections for 10 sequential requests, want 1", n)
	}
}

// A request blocked on the hub must not outlive the relay: stopping Serve
// cancels the outbound call and Serve returns well inside the 5s drain.
func TestRelayStopCancelsAHubCallThatIsBlocked(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	hubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		close(entered)
		select {
		case <-release:
		case <-req.Context().Done():
		}
	}))
	t.Cleanup(hubSrv.Close)
	t.Cleanup(func() { close(release) })
	tf, _ := tokenFile(t)
	r := &EventsRelay{Listen: "127.0.0.1:0", Hub: hubSrv.URL, TokenFile: tf}
	ctx, cancel := context.WithCancel(context.Background())
	addr := make(chan string, 1)
	done := make(chan error, 1)
	go func() { done <- r.Serve(ctx, func(a net.Addr) { addr <- a.String() }) }()
	a := <-addr
	reply := make(chan int, 1)
	go func() {
		res, err := http.Post("http://"+a+"/events", "application/json", strings.NewReader(oneEvent))
		if err != nil {
			reply <- 0
			return
		}
		_ = res.Body.Close()
		reply <- res.StatusCode
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("request never reached the hub")
	}
	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("Serve did not return while a hub call was blocked")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("Serve took %v to stop", d)
	}
	<-reply
}

// A client that sends headers and then stalls mid-body holds its handler and
// connection. Stopping the relay must cut it off after the grace, not leave it
// running behind a Serve that has already returned.
func TestRelayStopClosesASlowUploadAfterTheGrace(t *testing.T) {
	old := relayShutdownGrace
	relayShutdownGrace = 200 * time.Millisecond
	t.Cleanup(func() { relayShutdownGrace = old })
	hub := newHub(t)
	tf, _ := tokenFile(t)
	r := relayFor(t, hub, tf, nil)
	r.port = ""
	ctx, cancel := context.WithCancel(context.Background())
	addr := make(chan string, 1)
	done := make(chan error, 1)
	go func() { done <- r.Serve(ctx, func(a net.Addr) { addr <- a.String() }) }()
	a := <-addr
	conn, err := net.Dial("tcp", a)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// Declares 1000 bytes, sends one, then goes quiet.
	if _, err := fmt.Fprintf(conn, "POST /events HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nContent-Length: 1000\r\n\r\n{", a); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for r.inflight.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("handler never started")
		}
		time.Sleep(5 * time.Millisecond)
	}
	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not return with a stalled upload")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Serve took %v", d)
	}
	if n := r.inflight.Load(); n != 0 {
		t.Fatalf("%d handlers still running after Serve returned", n)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadAll(conn); err != nil && !errors.Is(err, io.EOF) {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			t.Fatal("the stalled connection is still open after Serve returned")
		}
	}
}

type failingListener struct{ net.Listener }

func (failingListener) Accept() (net.Conn, error) { return nil, errors.New("accept failed") }

// If the server fails on its own, Serve still cancels the relay's context and
// joins the shutdown watcher; nothing is left waiting on the caller's context.
func TestRelayServeErrorCancelsItsContextAndJoins(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hub := newHub(t)
	tf, _ := tokenFile(t)
	r := relayFor(t, hub, tf, nil)
	done := make(chan error, 1)
	go func() { done <- r.serve(context.Background(), failingListener{ln}, nil) }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "accept failed") {
			t.Fatalf("serve error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return after the listener failed (watcher not released)")
	}
	if life := r.lifeCtx(); life == nil || life.Err() == nil {
		t.Fatal("the relay's context was not canceled on a failed Serve")
	}
}
