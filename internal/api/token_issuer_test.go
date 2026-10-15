package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/authissuer"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/launchprofile"
)

type tokenFake map[string]identity.Principal

func (f tokenFake) Verify(_ context.Context, t string) (identity.Principal, error) {
	if p, ok := f[t]; ok {
		return p, nil
	}
	return identity.Principal{}, identity.ErrInvalidToken
}

func issuerFixture(t *testing.T, mode identity.Mode) (*authissuer.Issuer, http.Handler) {
	t.Helper()
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
	stored := tokenFake{
		"tth_dev":   {ID: "msg://device/d1", Kind: "device", Scopes: []string{"read", "operate"}},
		"tth_admin": {ID: "msg://device/d2", Kind: "device", Scopes: []string{"maintain", "admin"}},
		"tth_read":  {ID: "msg://device/d3", Kind: "device", Scopes: []string{"read"}},
	}
	v, err := authissuer.NewVerifier(stored, is)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	for _, p := range []string{TokenPath, DenyListPath, JWKSPath} {
		mux.Handle(p, NewTokenIssuerHandler(is))
	}
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	for _, p := range []string{"/sessions", "/sessions/", "/auth/pair", "/mcp", "/nothing-mapped"} {
		mux.Handle(p, ok)
	}
	return is, identity.Middleware(mode, v, nil, AgentScopeMiddleware(mode, mux))
}

func call(h http.Handler, method, path, bearer, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	out := httptest.NewRecorder()
	h.ServeHTTP(out, req)
	return out
}

func mint(t *testing.T, h http.Handler, bearer, body string) (int, authissuer.Token) {
	t.Helper()
	out := call(h, "POST", "/auth/token", bearer, body)
	var tok authissuer.Token
	_ = json.Unmarshal(out.Body.Bytes(), &tok)
	return out.Code, tok
}

func TestTokenEndpointJWKSAndDenyList(t *testing.T) {
	is, h := issuerFixture(t, identity.Enforce)
	// public JWKS, no bearer
	if out := call(h, "GET", JWKSPath, "", ""); out.Code != 200 || !strings.Contains(out.Body.String(), `"OKP"`) {
		t.Fatalf("jwks: %d %s", out.Code, out.Body)
	}
	// exchange
	code, tok := mint(t, h, "tth_dev", `{"audience":"tether","scopes":["read"],"ttl_seconds":120}`)
	if code != 200 || tok.TTL != 120 || tok.Kind != "device" {
		t.Fatalf("mint: %d %+v", code, tok)
	}
	if c, _ := mint(t, h, "tth_read", `{"audience":"tether","scopes":["operate"]}`); c != 403 {
		t.Fatalf("exceeding scopes: %d", c)
	}
	if c, _ := mint(t, h, "", `{"audience":"tether","scopes":["read"]}`); c != 401 {
		t.Fatalf("anonymous: %d", c)
	}
	// an issued token never mints, whatever its kind
	if c, _ := mint(t, h, tok.Token, `{"audience":"tether","scopes":["read"]}`); c != 403 {
		t.Fatalf("issued token minted: %d", c)
	}
	// deny-list: read-only device refused; maintain device served with ETag; 304 on match
	if out := call(h, "GET", DenyListPath, "tth_read", ""); out.Code != 403 {
		t.Fatalf("denylist without maintain: %d", out.Code)
	}
	if out := call(h, "GET", DenyListPath, tok.Token, ""); out.Code != 403 {
		t.Fatalf("denylist with a read-only issued token: %d", out.Code)
	}
	_ = is.State.Deny(authissuer.DenySID, "gone", time.Now().Add(time.Hour))
	out := call(h, "GET", DenyListPath, "tth_admin", "")
	etag := out.Header().Get("ETag")
	var doc struct{ JTI, Sub, SID []string }
	if out.Code != 200 || etag == "" || json.Unmarshal(out.Body.Bytes(), &doc) != nil || len(doc.SID) != 1 || doc.SID[0] != "gone" || doc.JTI == nil || doc.Sub == nil {
		t.Fatalf("denylist: %d %s", out.Code, out.Body)
	}
	req := httptest.NewRequest("GET", DenyListPath, nil)
	req.Header.Set("Authorization", "Bearer tth_admin")
	req.Header.Set("If-None-Match", etag)
	r2 := httptest.NewRecorder()
	h.ServeHTTP(r2, req)
	if r2.Code != 304 {
		t.Fatalf("etag revalidation: %d", r2.Code)
	}
	// writes need admin
	if out := call(h, "POST", DenyListPath, "tth_dev", `{"kind":"jti","value":"x"}`); out.Code != 403 {
		t.Fatalf("deny write without admin: %d", out.Code)
	}
	if out := call(h, "POST", DenyListPath, "tth_admin", `{"kind":"jti","value":"x"}`); out.Code != 200 || !is.State.Denied("jti", "x") {
		t.Fatalf("deny write: %d", out.Code)
	}
}

func TestIssuedTokensGetRemoteScopePolicyOnLocalListenerInEnforce(t *testing.T) {
	_, h := issuerFixture(t, identity.Enforce)
	_, read := mint(t, h, "tth_dev", `{"audience":"tether","scopes":["read"]}`)
	_, operate := mint(t, h, "tth_dev", `{"audience":"tether","scopes":["tether.operate"]}`)
	for _, c := range []struct {
		token, method, path string
		want                int
	}{
		{read.Token, "GET", "/sessions/s1/events", 204},
		{read.Token, "POST", "/sessions", 403},
		{operate.Token, "POST", "/sessions", 204},
		{operate.Token, "GET", "/sessions/s1/events", 403}, // independent scopes, no hierarchy
		{operate.Token, "POST", "/auth/pair", 403},         // local custody
		{operate.Token, "GET", "/mcp", 404},
		{read.Token, "GET", "/nothing-mapped", 404}, // fail closed
		{"tth_dev", "GET", "/nothing-mapped", 204},  // stored credentials keep today's local behavior
		{"tth_dev", "POST", "/sessions", 204},
	} {
		if got := call(h, c.method, c.path, c.token, "{}").Code; got != c.want {
			t.Errorf("%s %s: got %d want %d", c.method, c.path, got, c.want)
		}
	}
	// observe mode never rejects an issued token
	_, ho := issuerFixture(t, identity.Observe)
	_, rd := mint(t, ho, "tth_dev", `{"audience":"tether","scopes":["read"]}`)
	if got := call(ho, "POST", "/sessions", rd.Token, "{}").Code; got != 204 {
		t.Errorf("observe rejected: %d", got)
	}
}

func TestSelfScopeIsForDevicesOnly(t *testing.T) {
	op := identity.Principal{ID: identity.OperatorID, Kind: "operator", Scopes: []string{"*"}}
	if !WouldAllowLocal(op, selfScope) {
		t.Fatal("operator may mint locally")
	}
	if WouldAllowLocal(identity.Principal{Kind: "session", Scopes: []string{"message.write"}}, selfScope) {
		t.Fatal("session credential must not")
	}
}

// A default agent (launchprofile.DefaultMCPScopes) is issued tether.operate and
// tether.registry, never tether.maintain, and so cannot reach maintain-only
// routes.
func TestDefaultAgentScopesAreNarrow(t *testing.T) {
	is, h := issuerFixture(t, identity.Enforce)
	op := identity.Principal{Verified: true, ID: identity.OperatorID, Kind: "operator", Scopes: []string{"*"}}
	scopes, err := identity.CanonicalScopes(launchprofile.DefaultMCPScopes)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(scopes, " ") != "tether.operate tether.registry" {
		t.Fatalf("default scopes canonicalize to %v", scopes)
	}
	tok, err := is.Exchange(op, authissuer.Request{Audience: "tether", Scopes: launchprofile.DefaultMCPScopes, SessionID: "default-agent"})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range tok.Scopes {
		if s == "tether.maintain" || s == "tether.admin" {
			t.Fatalf("default agent carries %s", s)
		}
	}
	for _, c := range []struct {
		method, path string
		want         int
	}{
		{"POST", "/sessions", 204},               // operate
		{"POST", "/registry/bootstrap", 404},     // registry scope, but route not mounted in this fixture
		{"POST", "/catalog/reload", 403},         // local custody
		{"POST", "/catalog/plans/p1/apply", 403}, // local custody
		{"GET", "/auth/denylist", 403},           // needs maintain
		{"GET", "/logs/daemon", 404},             // maintain-only route: unmapped here, never reachable
		{"POST", "/settings/onboarding/x", 404},  // maintain-only
	} {
		got := call(h, c.method, c.path, tok.Token, "{}").Code
		if c.path == "/registry/bootstrap" || c.path == "/logs/daemon" || c.path == "/settings/onboarding/x" {
			if got == 204 {
				t.Errorf("%s %s reached a handler", c.method, c.path)
			}
			continue
		}
		if got != c.want {
			t.Errorf("%s %s: got %d want %d", c.method, c.path, got, c.want)
		}
	}
	// the scope gate itself, independent of mounting: registry yes, maintain no
	if !identity.ScopeGranted(tok.Scopes, "registry.write") || identity.ScopeGranted(tok.Scopes, identity.ScopeMaintain) {
		t.Fatalf("scopes %v", tok.Scopes)
	}
	// a maintain device still satisfies the registry route
	if !identity.HasDeviceScope(identity.Principal{ID: "d", Kind: "device", Scopes: []string{"maintain"}}, registryScope) {
		t.Fatal("maintain device must satisfy registry")
	}
}
