package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

type seenRequest struct{ method, path, auth string }

// Even a same-host redirect forwards Authorization by default. Resource calls
// must fail rather than follow it or treat the redirect response as success.
func TestCerberusEndpointRejectsRedirects(t *testing.T) {
	for _, status := range []int{301, 302, 303, 307, 308} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var redirected atomic.Int64
			var initial atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/redirected" {
					redirected.Add(1)
					w.WriteHeader(http.StatusOK)
					return
				}
				initial.Add(1)
				if r.Header.Get("Authorization") != "Bearer k-redirect" {
					t.Error("initial request missing configured bearer")
				}
				http.Redirect(w, r, "/redirected", status)
			}))
			t.Cleanup(srv.Close)
			t.Setenv(cerberusAddrEnv, srv.URL+"/api")
			t.Setenv(cerberusTokenFileEnv, tokenFile(t, "k-redirect"))
			for _, action := range []string{"status", "restart"} {
				if _, err := runCerberusSocketResourceAction(context.Background(), "tether-hub", action); err == nil || !strings.Contains(err.Error(), "cerberus redirects are not allowed") {
					t.Errorf("%s redirect error = %v", action, err)
				}
			}
			if initial.Load() != 2 || redirected.Load() != 0 {
				t.Fatalf("requests: initial=%d redirected=%d", initial.Load(), redirected.Load())
			}
		})
	}
}

// fakeCerberus answers resource actions like the old daemon and records what it was asked.
func fakeCerberus(t *testing.T, fail bool) (http.Handler, func() []seenRequest) {
	t.Helper()
	var mu sync.Mutex
	var seen []seenRequest
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, seenRequest{r.Method, r.URL.Path, r.Header.Get("Authorization")})
		mu.Unlock()
		if fail {
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": "forbidden", "message": "missing scope cerberus.operate", "details": map[string]any{"scope": "cerberus.operate"}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "status": "running"})
	})
	return h, func() []seenRequest { mu.Lock(); defer mu.Unlock(); return append([]seenRequest(nil), seen...) }
}

func serveUnix(t *testing.T, h http.Handler) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "cb") // short: unix socket paths are limited
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "cerberus.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	return sock
}

func tokenFile(t *testing.T, token string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "cerberus.token")
	if err := os.WriteFile(p, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// CW-20261114-0145: with neither setting, resource actions go to ~/.cerberus/cerberus.sock exactly as
// before, with no Authorization header.
func TestCerberusEndpointDefaultIsTheOldSocket(t *testing.T) {
	h, seen := fakeCerberus(t, false)
	sock := serveUnix(t, h)
	home, err := os.MkdirTemp("", "hm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	if err := os.MkdirAll(filepath.Join(home, ".cerberus"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(sock, filepath.Join(home, ".cerberus", "cerberus.sock")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv(cerberusAddrEnv, "")
	t.Setenv(cerberusTokenFileEnv, "")
	if _, err := runCerberusSocketResourceAction(context.Background(), "tether-hub", "restart"); err != nil {
		t.Fatal(err)
	}
	if got := seen(); len(got) != 1 || got[0] != (seenRequest{"POST", "/resources/tether-hub/restart", ""}) {
		t.Fatalf("requests = %+v", got)
	}
}

// A unix: address dials that socket; a token file adds the bearer.
func TestCerberusEndpointUnixAddressWithToken(t *testing.T) {
	h, seen := fakeCerberus(t, false)
	sock := serveUnix(t, h)
	t.Setenv(cerberusAddrEnv, "unix:"+sock)
	t.Setenv(cerberusTokenFileEnv, tokenFile(t, "k-unix"))
	if _, err := runCerberusSocketResourceAction(context.Background(), "tether-hub", "status"); err != nil {
		t.Fatal(err)
	}
	if got := seen(); len(got) != 1 || got[0] != (seenRequest{"GET", "/resources/tether-hub/status", "Bearer k-unix"}) {
		t.Fatalf("requests = %+v", got)
	}
}

// An http:// base (the new Cerberus, mounted at /api) gets <base>/resources/{id}/{action} with the
// bearer, the token is re-read on each call, and the `message` of its flat {code, message, details}
// error surfaces as the error (an extra field such as details is tolerated).
func TestCerberusEndpointHTTPWithToken(t *testing.T) {
	h, seen := fakeCerberus(t, false)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	tok := tokenFile(t, "k-one")
	t.Setenv(cerberusAddrEnv, srv.URL+"/api/")
	t.Setenv(cerberusTokenFileEnv, tok)
	if _, err := runCerberusSocketResourceAction(context.Background(), "tether-hub", "restart"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tok, []byte("k-two"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runCerberusSocketResourceAction(context.Background(), "tether-hub", "status"); err != nil {
		t.Fatal(err)
	}
	want := []seenRequest{{"POST", "/api/resources/tether-hub/restart", "Bearer k-one"}, {"GET", "/api/resources/tether-hub/status", "Bearer k-two"}}
	if got := seen(); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("requests = %+v", got)
	}

	failing, _ := fakeCerberus(t, true)
	bad := httptest.NewServer(failing)
	t.Cleanup(bad.Close)
	t.Setenv(cerberusAddrEnv, bad.URL+"/api")
	if _, err := runCerberusSocketResourceAction(context.Background(), "tether-hub", "restart"); err == nil || !strings.Contains(err.Error(), "missing scope cerberus.operate") {
		t.Fatalf("flat error = %v", err)
	}
}

func TestCerberusEndpointRejectsBadSettings(t *testing.T) {
	for _, addr := range []string{"tcp://x", "unix:relative.sock", "http://"} {
		t.Setenv(cerberusAddrEnv, addr)
		if _, err := resolveCerberusEndpoint(); err == nil {
			t.Fatalf("%q accepted", addr)
		}
	}
	h, seen := fakeCerberus(t, false)
	t.Setenv(cerberusAddrEnv, "unix:"+serveUnix(t, h))
	t.Setenv(cerberusTokenFileEnv, filepath.Join(t.TempDir(), "missing"))
	if _, err := runCerberusSocketResourceAction(context.Background(), "x", "status"); err == nil || !strings.Contains(err.Error(), cerberusTokenFileEnv) {
		t.Fatalf("a missing token file: %v", err)
	}
	if len(seen()) != 0 {
		t.Fatal("a request went out without the configured token")
	}
}

// @photon-review on #571: the bearer token must not cross a network in the clear. Plain http is allowed
// only to loopback or the tailnet; https anywhere; anything else is refused when the address is resolved.
func TestCerberusEndpointPlainHTTPOnlyToLoopbackOrTailnet(t *testing.T) {
	cases := map[string]bool{
		"http://127.0.0.1:4786/api":               true,
		"http://127.9.9.9:4786":                   true,
		"http://localhost:4786":                   true,
		"http://[::1]:4786":                       true,
		"http://[fd7a:115c:a1e0::1]:4786":         true,
		"http://node.tailnet-example.ts.net:4786": true,
		"https://cerberus.example.com/api":        true,
		"https://203.0.113.5:4786":                true,
		"http://203.0.113.5:4786":                 false,
		"http://10.0.0.5:4786":                    false,
		"http://100.128.0.1:4786":                 false,
		"http://cerberus.example.com":             false,
		"http://ts.net.evil.com":                  false,
		"http://[fd7a:115c:a1e1::1]:4786":         false,
		"ftp://127.0.0.1":                         false,
	}
	// The edges of the tailnet's CGNAT range, built at runtime so no tailnet address is a literal here.
	for addr, ok := range map[[4]byte]bool{{100, 64, 0, 0}: true, {100, 127, 255, 255}: true, {100, 63, 255, 255}: false, {100, 128, 0, 0}: false} {
		cases["http://"+netip.AddrFrom4(addr).String()+":4786"] = ok
	}
	for addr, ok := range cases {
		t.Setenv(cerberusAddrEnv, addr)
		_, err := resolveCerberusEndpoint()
		if ok && err != nil {
			t.Errorf("%s refused: %v", addr, err)
		}
		if !ok && err == nil {
			t.Errorf("%s accepted", addr)
		}
	}
}

// The default (unset) path keeps today's error wording.
func TestCerberusEndpointDefaultKeepsTheSocketErrorWording(t *testing.T) {
	sock := serveUnix(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) }))
	home, err := os.MkdirTemp("", "hm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	if err := os.MkdirAll(filepath.Join(home, ".cerberus"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(sock, filepath.Join(home, ".cerberus", "cerberus.sock")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv(cerberusAddrEnv, "")
	t.Setenv(cerberusTokenFileEnv, "")
	if _, err := runCerberusSocketResourceAction(context.Background(), "x", "restart"); err == nil || !strings.HasPrefix(err.Error(), "cerberus socket POST /resources/x/restart: http 500") {
		t.Fatalf("default-path error = %v", err)
	}
	t.Setenv("HOME", t.TempDir()) // no socket at all
	if _, err := runCerberusSocketResourceAction(context.Background(), "x", "status"); err == nil || !strings.HasPrefix(err.Error(), "cerberus socket not found at ") {
		t.Fatalf("missing socket error = %v", err)
	}
}
