package api

import (
	"context"
	"encoding/json"
	"errors"
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
	for _, p := range []string{"/sessions", "/sessions/", "/auth/pair", "/mcp", "/nothing-mapped", "/messages", "/groups"} {
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
	// deny-list: the minimal list (ids only) is PUBLIC: any caller, or none, reads
	// it with an ETag; 304 on match. Writing it still needs admin (below).
	// (The fixture wraps the handler in the identity middleware; the unauthenticated
	// path is mounted in the daemon and exercised by the daemon's remote listener
	// test and by TestPublicDenyListReadHandler.)
	for _, bearer := range []string{"tth_read", tok.Token} {
		if out := call(h, "GET", DenyListPath, bearer, ""); out.Code != 200 {
			t.Fatalf("public denylist read with bearer %q: %d", bearer, out.Code)
		}
	}
	_ = is.State.Deny(authissuer.DenySID, "gone", time.Now().Add(time.Hour))
	out := call(h, "GET", DenyListPath, "tth_read", "")
	etag := out.Header().Get("ETag")
	var doc struct{ JTI, Sub, SID []string }
	if out.Code != 200 || etag == "" || json.Unmarshal(out.Body.Bytes(), &doc) != nil || len(doc.SID) != 1 || doc.SID[0] != "gone" || doc.JTI == nil || doc.Sub == nil {
		t.Fatalf("denylist: %d %s", out.Code, out.Body)
	}
	req := httptest.NewRequest("GET", DenyListPath, nil)
	req.Header.Set("Authorization", "Bearer tth_read")
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

// A default agent (launchprofile.DefaultMCPScopes) holds only the narrow scopes
// it holds today (message, registry, groups): no operate, maintain or admin, so
// it cannot launch sessions, call the AI gateway, use the broker or teams, or
// reach maintain-only routes.
func TestDefaultAgentScopesAreNarrow(t *testing.T) {
	is, h := issuerFixture(t, identity.Enforce)
	op := identity.Principal{Verified: true, ID: identity.OperatorID, Kind: "operator", Scopes: []string{"*"}}
	scopes, err := identity.CanonicalScopes(launchprofile.DefaultMCPScopes)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(scopes, " ") != "tether.groups tether.messages tether.registry" {
		t.Fatalf("default scopes canonicalize to %v", scopes)
	}
	tok, err := is.Exchange(op, authissuer.Request{Audience: "tether", Scopes: launchprofile.DefaultMCPScopes, SessionID: "default-agent"})
	if err != nil {
		t.Fatal(err)
	}
	if identity.ScopeGranted(tok.Scopes, identity.ScopeEvents) {
		t.Fatal("default agent gained event publication")
	}
	for _, s := range tok.Scopes {
		switch s {
		case "tether.operate", "tether.maintain", "tether.admin", "tether.terminal":
			t.Fatalf("default agent carries %s", s)
		}
	}
	for _, c := range []struct {
		method, path string
		want         int
	}{
		{"POST", "/messages", 204},         // message.write
		{"POST", "/groups", 204},           // groups.write
		{"POST", "/sessions", 403},         // session launch needs tether.sessions
		{"POST", "/ai/chat", 404},          // gated by ai (unmounted here: never a handler)
		{"POST", "/teams/form", 404},       // teams (unmounted here)
		{"POST", "/broker/envelopes", 404}, // broker stays operate
		{"POST", "/catalog/reload", 403},
		{"POST", "/catalog/plans/p1/apply", 403},
		{"GET", "/auth/denylist", 200}, // public minimal list
	} {
		got := call(h, c.method, c.path, tok.Token, "{}").Code
		if got == 204 && c.want != 204 {
			t.Errorf("%s %s reached a handler", c.method, c.path)
		}
		if c.want == 204 || c.want == 403 {
			if got != c.want {
				t.Errorf("%s %s: got %d want %d", c.method, c.path, got, c.want)
			}
		}
	}
	// the gate itself, independent of what the fixture mounts
	for _, route := range [][2]string{{"POST", "/ai/chat"}, {"POST", "/ai/embeddings"}, {"POST", "/ai/chat/stream"}, {"POST", "/teams/form"}, {"POST", "/broker/envelopes"}, {"POST", "/sessions"}, {"POST", "/registry/bootstrap"}, {"GET", "/logs/daemon"}} {
		scope, _, known := RequiredRemoteScope(httptest.NewRequest(route[0], route[1], nil))
		if !known {
			t.Fatalf("%v unmapped", route)
		}
		want := route[1] == "/registry/bootstrap"
		if got := identity.ScopeGranted(tok.Scopes, scope); got != want {
			t.Errorf("%v requires %q: granted=%v want %v", route, scope, got, want)
		}
	}
	// a maintain device still satisfies the registry route, and operate the narrow ones
	if !identity.HasDeviceScope(identity.Principal{ID: "d", Kind: "device", Scopes: []string{"maintain"}}, registryScope) {
		t.Fatal("maintain device must satisfy registry")
	}
	for _, sc := range []string{sessionsScope, messagesScope, groupsScope, teamsScope, aiScope} {
		if !identity.HasDeviceScope(identity.Principal{ID: "d", Kind: "device", Scopes: []string{"operate"}}, sc) {
			t.Fatalf("operate device must satisfy %s", sc)
		}
	}
}

// Each narrow scope reaches exactly the routes its legacy scope covered and no
// other narrow scope's routes.
func TestNarrowScopesReachOnlyTheirOwnRoutes(t *testing.T) {
	routes := map[string][][2]string{
		sessionsScope: {{"POST", "/sessions"}, {"POST", "/sessions/s1/launch"}, {"POST", "/sessions/s1/stop"}, {"POST", "/sessions/s1/turn"}, {"POST", "/sessions/s1/input"}, {"POST", "/sessions/s1/resize"}, {"POST", "/sessions/s1/workstream"}, {"POST", "/sessions/s1/refs"}, {"POST", "/logical-agents/a1/resume"}, {"POST", "/workstreams"}, {"POST", "/workstreams/w1/sessions"}},
		messagesScope: {{"POST", "/messages"}, {"POST", "/messages/notify"}, {"POST", "/messages/m1/consume"}, {"POST", "/messages/m1/cancel"}, {"POST", "/messages/m1/read"}, {"POST", "/messages/m1/archive"}, {"POST", "/messages/m1/reply"}},
		groupsScope:   {{"POST", "/groups"}, {"POST", "/groups/g1/members"}, {"POST", "/groups/g1/messages"}, {"POST", "/groups/g1/leave"}, {"POST", "/groups/g1/read"}, {"DELETE", "/groups/g1"}},
		teamsScope:    {{"POST", "/teams/form"}, {"POST", "/teams/assign"}, {"POST", "/teams/report_result"}},
		aiScope:       {{"POST", "/ai/chat"}, {"POST", "/ai/chat/stream"}, {"POST", "/ai/embeddings"}, {"POST", "/ai/decide"}},
		registryScope: {{"POST", "/registry/bootstrap"}, {"POST", "/registry/scoped-bindings"}, {"PATCH", "/registry/agents/a1"}},
	}
	for scope, mine := range routes {
		granted := []string{"tether." + scope}
		for owner, rs := range routes {
			for _, r := range rs {
				need, _, known := RequiredRemoteScope(httptest.NewRequest(r[0], r[1], nil))
				if !known {
					t.Fatalf("%v unmapped", r)
				}
				got := identity.ScopeGranted(granted, need)
				// session input/resize are also reachable by tether.sessions only
				want := owner == scope
				if got != want {
					t.Errorf("tether.%s on %s %s (%s's route, needs %q): got %v want %v", scope, r[0], r[1], owner, need, got, want)
				}
			}
		}
		_ = mine
	}
	// none of them reaches an operate-level route outside its set
	for _, r := range [][2]string{{"POST", "/broker/envelopes"}, {"POST", "/session-groups"}, {"POST", "/sessions/s1/checkpoint"}, {"POST", "/chat/agents/l1/send"}, {"GET", "/sessions/s1/log"}, {"POST", "/catalog/validate"}} {
		need, _, _ := RequiredRemoteScope(httptest.NewRequest(r[0], r[1], nil))
		for scope := range routes {
			if identity.ScopeGranted([]string{"tether." + scope}, need) && need != readScope {
				t.Errorf("tether.%s reached %s %s (needs %q)", scope, r[0], r[1], need)
			}
		}
	}
}

func TestEventsScopeMintingAndReadsRemainIndependent(t *testing.T) {
	is, h := issuerFixture(t, identity.Enforce)
	for _, tc := range []struct {
		name, kind, grant string
		allowed           bool
	}{
		{"service explicit", "service", "tether.events", true},
		{"service operate", "service", "tether.operate", true},
		{"worker", "device", "worker", true},
		{"worker canonical", "device", "tether.worker", true},
		{"admin", "service", "tether.admin", false},
		{"read", "device", "read", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent := identity.Principal{Verified: true, ID: "event-publisher", Kind: tc.kind, Scopes: []string{tc.grant}}
			token, err := is.Exchange(parent, authissuer.Request{Audience: "tether", Scopes: []string{"events"}})
			if !tc.allowed {
				if !errors.Is(err, authissuer.ErrForbidden) {
					t.Fatalf("wanted forbidden, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if token.Kind != tc.kind || len(token.Scopes) != 1 || token.Scopes[0] != "tether.events" {
				t.Fatalf("minted kind=%s scopes=%v", token.Kind, token.Scopes)
			}
			if got := call(h, "GET", "/sessions/s1/events", token.Token, "").Code; got != http.StatusForbidden {
				t.Fatalf("publication token read events: %d", got)
			}
		})
	}
	code, read := mint(t, h, "tth_read", `{"audience":"tether","scopes":["read"]}`)
	if code != http.StatusOK || call(h, "GET", "/sessions/s1/events", read.Token, "").Code != http.StatusNoContent {
		t.Fatal("read token lost event read access")
	}
	if !launchprofile.ValidMCPScope("tether.events") {
		t.Fatal("explicit event grant rejected by launch profile")
	}
}
