package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/store"
)

// Every service-token management route the issuer handler serves must be
// registered on the daemon's router. ServeMux matches these paths exactly, and
// a route missing here answers the mux's plain 404 instead of reaching the
// handler: revoke and rotate-key were unreachable on a live hub that way. With
// no caller identity the handler answers 401 with a JSON error, so a 404 or a
// non-JSON body means the request never got there.
func TestServiceTokenManagementRoutesReachTheIssuerHandler(t *testing.T) {
	s, _, _ := remoteTestServer(t)
	s.Issuer = serviceIssuer(t)
	h := s.Handler()
	for _, rt := range []struct{ method, path string }{
		{"POST", "/auth/service-tokens"},
		{"GET", "/auth/service-tokens"},
		{"POST", "/auth/service-tokens/revoke"},
		{"POST", "/auth/service-tokens/rotate-key"},
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(rt.method, rt.path, strings.NewReader("{}")))
		if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), `"unauthorized"`) {
			t.Errorf("%s %s: %d %q, want the handler's 401", rt.method, rt.path, w.Code, w.Body.String())
		}
	}
}

// Registering the subpaths must not open them to the remote listener: the
// operator's own credential is refused there too, on every management route.
func TestServiceTokenManagementRoutesStayLocalOnly(t *testing.T) {
	s, _, operator := remoteTestServer(t)
	s.Issuer = serviceIssuer(t)
	h := s.RemoteHandler("127.0.0.1:7331")
	for _, rt := range []struct{ method, path string }{
		{"POST", "/auth/service-tokens"},
		{"GET", "/auth/service-tokens"},
		{"POST", "/auth/service-tokens/revoke"},
		{"POST", "/auth/service-tokens/rotate-key"},
	} {
		r := httptest.NewRequest(rt.method, "http://localhost:9000"+rt.path, strings.NewReader(`{"jti":"x"}`))
		r.Header.Set("Authorization", "Bearer "+operator)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden && w.Code != http.StatusNotFound && w.Code != http.StatusUnauthorized {
			t.Errorf("remote %s %s with the operator credential: %d %q", rt.method, rt.path, w.Code, w.Body.String())
		}
	}
}

// The whole management surface works end to end through the real daemon router
// and identity middleware, over a unix socket as the operator: issue, list,
// revoke by jti, revoke --all, rotate-key. Handler-level tests (authissuer, api)
// call the issuer handler directly and cannot see a route the router lacks.
func TestServiceTokenManagementWorksThroughTheDaemonRouter(t *testing.T) {
	s, _, operator := remoteTestServer(t)
	s.Issuer = serviceIssuer(t)
	dir, err := os.MkdirTemp("", "stk")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: s.Handler(), ConnContext: identity.ConnectionContext}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}}
	do := func(method, path, body string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(method, "http://tether"+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+operator)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	issue := func(app string) string {
		t.Helper()
		code, body := do("POST", "/auth/service-tokens", `{"app":"`+app+`","audience":"tether","scopes":["tether.events"]}`)
		var out struct {
			JTI string `json:"jti"`
		}
		_ = json.Unmarshal([]byte(body), &out)
		if code != http.StatusOK && code != http.StatusCreated || out.JTI == "" {
			t.Fatalf("issue %s: %d %s", app, code, body)
		}
		return out.JTI
	}
	live := func() []string {
		t.Helper()
		code, body := do("GET", "/auth/service-tokens", "")
		var out struct {
			Tokens []struct {
				JTI       string     `json:"jti"`
				RevokedAt *time.Time `json:"revoked_at"`
			} `json:"tokens"`
		}
		if code != http.StatusOK || json.Unmarshal([]byte(body), &out) != nil {
			t.Fatalf("list: %d %s", code, body)
		}
		var ids []string
		for _, tk := range out.Tokens {
			if tk.RevokedAt == nil {
				ids = append(ids, tk.JTI)
			}
		}
		return ids
	}

	a, b, c := issue("cerberus"), issue("torque"), issue("torque")
	if got := live(); len(got) != 3 {
		t.Fatalf("live after issue: %v", got)
	}
	if code, body := do("POST", "/auth/service-tokens/revoke", `{"jti":"`+a+`"}`); code != http.StatusOK {
		t.Fatalf("revoke by jti: %d %s", code, body)
	}
	if got := live(); len(got) != 2 {
		t.Fatalf("live after revoke %s: %v", a, got)
	}
	if code, body := do("POST", "/auth/service-tokens/revoke", `{"app":"torque"}`); code != http.StatusOK {
		t.Fatalf("revoke by app: %d %s", code, body)
	}
	if got := live(); len(got) != 0 {
		t.Fatalf("live after revoking %s and %s by app: %v", b, c, got)
	}
	issue("cerberus")
	issue("torque")
	if code, body := do("POST", "/auth/service-tokens/revoke", `{"all":true}`); code != http.StatusOK {
		t.Fatalf("revoke --all: %d %s", code, body)
	}
	if got := live(); len(got) != 0 {
		t.Fatalf("live after revoke --all: %v", got)
	}
	issue("cerberus")
	if code, body := do("POST", "/auth/service-tokens/rotate-key", `{}`); code != http.StatusOK {
		t.Fatalf("rotate-key: %d %s", code, body)
	}
}

// POST /auth/service-tokens/request (CW-20261020-0036) is on the daemon's local router
// only when it can load sessions, answers from the handler (not the mux), and is
// refused on the remote listener even with the operator credential.
func TestServiceTokenRequestRouteWiring(t *testing.T) {
	s, _, operator := remoteTestServer(t)
	s.Issuer = serviceIssuer(t)
	post := func(h http.Handler, bearer string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "http://localhost:9000/auth/service-tokens/request", strings.NewReader(`{}`))
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	// Without a session lookup the route is not wired at all: it fails closed.
	if w := post(s.Handler(), ""); w.Code != http.StatusNotFound {
		t.Fatalf("unwired route: %d %q", w.Code, w.Body.String())
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "req.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s.SessionBootstrap = db
	if w := post(s.Handler(), ""); w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), `"unauthorized"`) {
		t.Fatalf("wired route with no identity: %d %q, want the handler's 401", w.Code, w.Body.String())
	}
	if w := post(s.RemoteHandler("127.0.0.1:7331"), operator); w.Code != http.StatusUnauthorized && w.Code != http.StatusForbidden && w.Code != http.StatusNotFound {
		t.Fatalf("remote listener with the operator credential: %d %q", w.Code, w.Body.String())
	}
}
