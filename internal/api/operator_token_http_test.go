package api

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/authissuer"
	"github.com/hollis-labs/tether/internal/identity"
)

// Round trip over a real unix socket in observe mode (the default): an
// operator mints a narrow tether.read token, and that token must not be able
// to use the operator-only /auth/pair or the deny-list.
func TestOperatorMintedNarrowTokenCannotPairOverHTTP(t *testing.T) {
	ids, _, opTok, router := deviceAuthFixture(t)
	dir := filepath.Join(t.TempDir(), "auth")
	ks, err := authissuer.OpenKeyStore(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	st, err := authissuer.OpenState(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	is := authissuer.New("tether", ks, st)
	v, err := authissuer.NewVerifier(ids, is)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/auth/token", NewTokenIssuerHandler(is))
	mux.Handle(DenyListPath, NewTokenIssuerHandler(is))
	mux.Handle("/", router)
	h := identity.Middleware(identity.Observe, v, nil, AgentScopeMiddleware(identity.Observe, mux))

	sock := filepath.Join(t.TempDir(), "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h, ConnContext: identity.ConnectionContext}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}}
	do := func(method, path, bearer, body string) (int, []byte) {
		t.Helper()
		req, _ := http.NewRequest(method, "http://tether"+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+bearer)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}

	// control: the real operator can pair on the unix socket
	if code, _ := do("POST", "/auth/pair", opTok, `{"scopes":["read"]}`); code != 200 && code != 201 {
		t.Fatalf("operator /auth/pair control returned %d", code)
	}
	code, body := do("POST", "/auth/token", opTok, `{"audience":"tether","scopes":["tether.read"],"ttl_seconds":900}`)
	var tok authissuer.Token
	_ = json.Unmarshal(body, &tok)
	if code != 200 || tok.Token == "" {
		t.Fatalf("operator mint: %d %s", code, body)
	}
	for _, c := range []struct{ method, path, body string }{
		{"POST", "/auth/pair", `{"scopes":["admin"]}`},
		{"POST", "/auth/pair/revoke", `{}`},
		{"GET", DenyListPath, ""},
		{"POST", DenyListPath, `{"sub":"x"}`},
	} {
		if code, b := do(c.method, c.path, tok.Token, c.body); code != 401 && code != 403 {
			t.Errorf("narrow token %s %s returned %d: %s", c.method, c.path, code, b)
		}
	}
}
