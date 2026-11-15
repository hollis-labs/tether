// Package spike is the S4 framework spike (CW-20261114-0181): two listeners, a peer-credential
// check, Raw mounts with the ADR 0010 envelope, SSE drain, and what a custom remote handler must do
// for itself on httphost. Throwaway: each test asserts what the framework does TODAY, so a finding
// is reproducible; S4 replaces this package with the real listener code.
package spike

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	libauth "github.com/hollis-labs/libs/security/auth"
	"github.com/hollis-labs/substrate/boot"
	"github.com/hollis-labs/substrate/module"
	"github.com/hollis-labs/substrate/modules/auth"
	"github.com/hollis-labs/substrate/modules/httphost"

	_ "github.com/hollis-labs/substrate/modules/config"
	_ "github.com/hollis-labs/substrate/modules/health"
	_ "github.com/hollis-labs/substrate/modules/logging"
)

const apiKey = "spike-key-0123456789"

// start boots config, logging, health, auth (in mode) and httphost.
func start(t *testing.T, mode string) *module.App {
	t.Helper()
	cfg := map[string]any{
		"logging": map[string]any{"level": "warn", "format": "text"},
		"auth": map[string]any{"mode": mode, "api_keys": []any{map[string]any{
			"id": "spike", "hash": libauth.HashKey(apiKey), "scopes": []any{"tether.read"}}}},
	}
	b, _ := json.Marshal(cfg)
	run, err := boot.Start(context.Background(), "tether", []string{"config", "logging", "health", "auth", "httphost"}, string(b))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run.Stop(context.Background()) })
	return run.App
}

// envelope is the ADR 0010 error body.
func envelope(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": msg}})
}

func isEnvelope(body string) bool {
	var v struct {
		Error *struct{ Code, Message string } `json:"error"`
	}
	return json.Unmarshal([]byte(body), &v) == nil && v.Error != nil && v.Error.Code != ""
}

func get(t *testing.T, c *http.Client, url string, hdr map[string]string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, "transport error: " + err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, strings.TrimSpace(string(b))
}

// F1: Serve cannot bind a unix socket, although ValidateBind accepts one.
func TestF1ServeRefusesUnixAddress(t *testing.T) {
	app := start(t, "permissive")
	sock := filepath.Join(t.TempDir(), "tetherd.sock")
	addr := "unix:" + sock
	if err := auth.ValidateBind(app, addr); err != nil {
		t.Fatalf("ValidateBind(%s) = %v; expected the framework to accept a unix socket", addr, err)
	}
	h, _ := httphost.Handler(app, httphost.Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := httphost.Serve(ctx, app, h, httphost.ServeOptions{Addr: addr})
	if err == nil {
		t.Fatal("Serve on a unix address returned nil")
	}
	t.Logf("FINDING F1: ValidateBind accepts %q but Serve fails: %v", "unix:<path>", err)
	if _, statErr := os.Stat(sock); statErr == nil {
		t.Fatal("unexpected: a socket file was created")
	}
}

// F2: two Serve calls in one process (two TCP listeners, two handlers) work, and each one's
// Stopping fires and drains an open SSE stream on cancel.
func TestF2TwoServeCallsDrainStreams(t *testing.T) {
	app := start(t, "permissive")
	sse := func(name string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "event: hello\ndata: %s\n\n", name)
			w.(http.Flusher).Flush()
			select {
			case <-httphost.Stopping(r):
				fmt.Fprint(w, "event: close\ndata: stopping\n\n")
				w.(http.Flusher).Flush()
			case <-r.Context().Done():
			}
		})
	}
	local, err := httphost.Handler(app, httphost.Options{Mounts: []httphost.Mount{{Pattern: "GET /events/stream", Handler: sse("local"), Open: true, Raw: true}}})
	if err != nil {
		t.Fatal(err)
	}
	remoteMux := http.NewServeMux()
	remoteMux.Handle("GET /events/stream", sse("remote"))
	ctx, cancel := context.WithCancel(context.Background())
	addrs := make(chan string, 2)
	done := make(chan error, 2)
	for _, h := range []http.Handler{local, remoteMux} {
		go func(h http.Handler) {
			done <- httphost.Serve(ctx, app, h, httphost.ServeOptions{Addr: "127.0.0.1:0", GracePeriod: 3 * time.Second,
				Ready: func(a net.Addr) { addrs <- a.String() }})
		}(h)
	}
	var lines []chan string
	for i := 0; i < 2; i++ {
		a := <-addrs
		resp, err := http.Get("http://" + a + "/events/stream")
		if err != nil {
			t.Fatal(err)
		}
		ch := make(chan string, 1)
		go func() {
			defer resp.Body.Close()
			var all []string
			s := bufio.NewScanner(resp.Body)
			for s.Scan() {
				all = append(all, s.Text())
			}
			ch <- strings.Join(all, "|")
		}()
		lines = append(lines, ch)
	}
	time.Sleep(100 * time.Millisecond)
	cancel()
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatalf("Serve returned %v; want a clean drain", err)
		}
	}
	for _, ch := range lines {
		got := <-ch
		if !strings.Contains(got, "event: close") {
			t.Fatalf("stream did not get the close frame: %q", got)
		}
	}
	t.Log("FINDING F2 (works): two Serve calls in one process; Stopping fires on each; both SSE streams got their close frame; both Serve returned nil")
}

// peerCredKey carries the unix peer credentials of a connection.
type peerCredKey struct{}

// F3: the socket on the app's own http.Server: the peer-credential check works through
// ConnContext, but Stopping(r) is nil there, so SSE drain and handler tracking are lost.
func TestF3SocketOnOwnServerLosesStopping(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var sawStopping bool
	var peer *syscall.Ucred
	srv := &http.Server{
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			uc, ok := c.(*net.UnixConn)
			if !ok {
				return ctx
			}
			raw, err := uc.SyscallConn()
			if err != nil {
				return ctx
			}
			var cred *syscall.Ucred
			_ = raw.Control(func(fd uintptr) { cred, _ = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED) })
			return context.WithValue(ctx, peerCredKey{}, cred)
		},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			peer, _ = r.Context().Value(peerCredKey{}).(*syscall.Ucred)
			sawStopping = httphost.Stopping(r) != nil
			if peer == nil || int(peer.Uid) != os.Getuid() {
				envelope(w, http.StatusForbidden, "forbidden", "peer is not the daemon's user")
				return
			}
			w.WriteHeader(http.StatusNoContent)
		}),
	}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()
	c := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}}
	code, body := get(t, c, "http://unix/x", nil)
	if code != http.StatusNoContent || peer == nil {
		t.Fatalf("peer-cred request: %d %q peer=%v", code, body, peer)
	}
	if sawStopping {
		t.Fatal("unexpected: Stopping is set outside httphost.Serve")
	}
	t.Logf("FINDING F3: peer credentials via ConnContext work (uid=%d pid>0=%v), but only on the app's own http.Server; there Stopping(r) is nil and Serve's handler tracking/drain do not apply", peer.Uid, peer.Pid > 0)
}

// F4: envelope behavior on httphost.Handler for a Raw mount.
func TestF4RawMountEnvelope(t *testing.T) {
	app := start(t, "enforce")
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/messages/missing" {
			envelope(w, http.StatusNotFound, "not_found", "message not found")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	auth.ScopeTable(app).Route("GET", "/messages/*", "tether.read")
	h, err := httphost.Handler(app, httphost.Options{Mounts: []httphost.Mount{
		{Pattern: "/messages/", Handler: ok, Raw: true},                             // guarded, raw
		{Pattern: "GET /.well-known/jwks.json", Handler: ok, Open: true, Raw: true}, // open, raw
		{Pattern: "GET /sessions/", Handler: ok, Raw: true},                         // guarded, raw, NOT in the scope table
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addrc := make(chan string, 1)
	go func() {
		_ = httphost.Serve(ctx, app, h, httphost.ServeOptions{Addr: "127.0.0.1:0", Ready: func(a net.Addr) { addrc <- a.String() }})
	}()
	base := "http://" + <-addrc
	bearer := map[string]string{"Authorization": "Bearer " + apiKey}
	cases := []struct {
		name      string
		path      string
		hdr       map[string]string
		method    string
		wantEnv   bool
		wantFound bool
	}{
		{"handler error passes through untouched", "/messages/missing", bearer, "GET", true, true},
		{"guard: no credential", "/messages/x", nil, "GET", false, true},
		{"guard: route not mapped", "/sessions/x", bearer, "GET", false, true},
		{"unknown path falls to the SPA host", "/nope", bearer, "GET", false, true},
		{"method not allowed on a method pattern", "/.well-known/jwks.json", nil, "POST", false, true},
	}
	for _, c := range cases {
		req, _ := http.NewRequest(c.method, base+c.path, nil)
		for k, v := range c.hdr {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		body := strings.TrimSpace(string(b))
		if isEnvelope(body) != c.wantEnv {
			t.Errorf("%s: status %d body %q: ADR 0010 envelope=%v, want %v", c.name, resp.StatusCode, body, isEnvelope(body), c.wantEnv)
		}
		t.Logf("FINDING F4 %-40s %s %s -> %d %.120s", c.name, c.method, c.path, resp.StatusCode, body)
	}
}

// F5: one auth guard per app, one mode. D6 needs the socket permissive and the remote listener
// enforcing in the same process.
func TestF5OneGuardModePerApp(t *testing.T) {
	app := start(t, "permissive")
	g := auth.Guard(app)
	if g == nil || g.Mode() != libauth.ModePermissive {
		t.Fatalf("guard = %v", g)
	}
	var called bool
	h := auth.Middleware(app, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true; w.WriteHeader(204) }))
	auth.ScopeTable(app).Route("GET", "/x", "tether.read")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addrc := make(chan string, 1)
	go func() {
		_ = httphost.Serve(ctx, app, h, httphost.ServeOptions{Addr: "127.0.0.1:0", Ready: func(a net.Addr) { addrc <- a.String() }})
	}()
	code, body := get(t, http.DefaultClient, "http://"+<-addrc+"/x", nil)
	if code != 204 || !called {
		t.Fatalf("permissive guard on the 'remote' listener: %d %q", code, body)
	}
	t.Log("FINDING F5: auth.Middleware wraps a custom handler, but with the app's single mode: a permissive socket means a permissive remote listener (no per-listener mode, no second guard)")
}

// F6: what a custom remote handler gets from the framework when it is not httphost.Handler.
func TestF6CustomRemoteHandlerGetsNothing(t *testing.T) {
	app := start(t, "permissive")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /boom", func(http.ResponseWriter, *http.Request) { panic("boom") })
	mux.HandleFunc("POST /echo", func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		fmt.Fprint(w, n)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addrc := make(chan string, 1)
	go func() {
		_ = httphost.Serve(ctx, app, mux, httphost.ServeOptions{Addr: "127.0.0.1:0", Ready: func(a net.Addr) { addrc <- a.String() }})
	}()
	base := "http://" + <-addrc
	// Host check: any Host header is served.
	req, _ := http.NewRequest("POST", base+"/echo", strings.NewReader(strings.Repeat("x", 9<<20)))
	req.Host = "attacker.invalid"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	t.Logf("FINDING F6a: foreign Host header served: %d; 9 MiB body read in full (%s bytes): no Host check and no body limit", resp.StatusCode, strings.TrimSpace(string(b)))
	// Panic: the connection is dropped, no envelope.
	_, err = http.Get(base + "/boom")
	if err == nil {
		t.Fatal("unexpected: a panicking handler produced a response")
	}
	t.Logf("FINDING F6b: a panic drops the connection (%v): no recovery, no envelope, logged by net/http with a stack", errors.Unwrap(err))
}

// F7: the same three questions for httphost.Handler itself (the socket's handler).
func TestF7HttphostHandlerHostPanicBody(t *testing.T) {
	app := start(t, "permissive")
	h, err := httphost.Handler(app, httphost.Options{Mounts: []httphost.Mount{
		{Pattern: "GET /boom", Open: true, Raw: true, Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") })},
		{Pattern: "POST /echo", Open: true, Raw: true, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n, _ := io.Copy(io.Discard, r.Body)
			fmt.Fprint(w, n)
		})},
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addrc := make(chan string, 1)
	go func() {
		_ = httphost.Serve(ctx, app, h, httphost.ServeOptions{Addr: "127.0.0.1:0", Ready: func(a net.Addr) { addrc <- a.String() }})
	}()
	base := "http://" + <-addrc
	req, _ := http.NewRequest("POST", base+"/echo", strings.NewReader(strings.Repeat("x", 9<<20)))
	req.Host = "attacker.invalid"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	t.Logf("FINDING F7a: httphost.Handler: foreign Host -> %d, 9 MiB body read (%s bytes): no Host check, no body limit", resp.StatusCode, strings.TrimSpace(string(b)))
	_, err = http.Get(base + "/boom")
	t.Logf("FINDING F7b: httphost.Handler: panic in a mount -> client error %v (no recovery, no envelope)", err)
}
