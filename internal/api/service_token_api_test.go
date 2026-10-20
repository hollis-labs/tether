package api

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	auth "github.com/hollis-labs/libs/security/auth"

	"github.com/hollis-labs/tether/internal/authissuer"
	"github.com/hollis-labs/tether/internal/identity"
)

func serviceTokenHandler(t *testing.T) (http.Handler, *authissuer.Issuer) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "auth")
	ks, err := authissuer.OpenKeyStore(dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	st, err := authissuer.OpenState(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	is := authissuer.New("tether", ks, st)
	return NewTokenIssuerHandler(is), is
}

// call runs a request as p; local says whether the connection is the local Unix socket.
func callServiceTokens(h http.Handler, method, body string, p *identity.Principal, local bool) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, ServiceTokensPath, strings.NewReader(body))
	ctx := r.Context()
	if local {
		ctx = identity.ConnectionContext(ctx, &net.UnixConn{})
	}
	if p != nil {
		ctx = identity.WithPrincipal(ctx, *p)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r.WithContext(ctx))
	return w
}

var opP = identity.Principal{Verified: true, ID: identity.OperatorID, Kind: "operator", Scopes: []string{"*"}}

func TestServiceTokenIssueAndListAreOperatorLocalOnly(t *testing.T) {
	h, is := serviceTokenHandler(t)
	body := `{"app":"cerberus","audience":"tether","scopes":["events"],"label":"agent-os"}`
	device := identity.Principal{Verified: true, ID: "msg://device/d", Kind: "device", Scopes: []string{"admin", "maintain", "operate"}}
	service := identity.Principal{Verified: true, ID: "msg://service/x/dev", Kind: "service", Scopes: []string{"*"}}
	agent := identity.Principal{Verified: true, ID: "msg://session/local/s", Kind: "agent", Scopes: []string{"*"}}
	narrow := opP
	narrow.TokenID = "j"
	for _, c := range []struct {
		name  string
		p     *identity.Principal
		local bool
		want  int
	}{
		{"anonymous", nil, true, 401},
		{"operator over a remote connection", &opP, false, 403},
		{"narrowed operator token", &narrow, true, 403},
		{"device", &device, true, 403},
		{"service", &service, true, 403},
		{"agent", &agent, true, 403},
	} {
		for _, method := range []string{http.MethodPost, http.MethodGet} {
			if w := callServiceTokens(h, method, body, c.p, c.local); w.Code != c.want {
				t.Errorf("%s %s: %d %s", c.name, method, w.Code, w.Body)
			}
		}
	}
	if len(is.State.Services()) != 0 {
		t.Fatal("a refused request recorded a token")
	}

	w := callServiceTokens(h, http.MethodPost, body, &opP, true)
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("issue: %d %s", w.Code, w.Body)
	}
	var out ServiceTokenResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.Token.Token == "" || out.Record.Sub != "msg://service/cerberus/dev" || out.Kind != "service" {
		t.Fatalf("%v %s", err, w.Body)
	}

	w = callServiceTokens(h, http.MethodGet, "", &opP, true)
	if w.Code != 200 || strings.Contains(w.Body.String(), out.Token.Token) {
		t.Fatalf("list: %d (leaks token: %v)", w.Code, strings.Contains(w.Body.String(), out.Token.Token))
	}
	var list ServiceTokenListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || len(list.Tokens) != 1 || list.Tokens[0].JTI != out.Record.JTI {
		t.Fatalf("%v %s", err, w.Body)
	}
}

func TestServiceTokenIssueRejectsBadRequests(t *testing.T) {
	h, _ := serviceTokenHandler(t)
	for _, body := range []string{
		`{}`,
		`{"app":"cerberus","audience":"tether","scopes":["admin"]}`,
		`{"app":"cerberus","audience":"tether","scopes":["tether.operate"]}`,
		`{"app":"Bad App","audience":"tether","scopes":["events"]}`,
		`{"app":"tachyon","audience":"tachyon","scopes":["tachyon.*"]}`,
		`not json`,
	} {
		if w := callServiceTokens(h, http.MethodPost, body, &opP, true); w.Code != 400 {
			t.Errorf("%s: %d %s", body, w.Code, w.Body)
		}
	}
	if w := callServiceTokens(h, http.MethodPost, `{"app":"tachyon","audience":"tachyon","scopes":["tachyon.*"],"allow_wildcard":true}`, &opP, true); w.Code != 200 {
		t.Errorf("dev wildcard: %d %s", w.Code, w.Body)
	}
}

func callRevoke(h http.Handler, body string, p *identity.Principal, local bool) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, ServiceTokensPath+"/revoke", strings.NewReader(body))
	ctx := r.Context()
	if local {
		ctx = identity.ConnectionContext(ctx, &net.UnixConn{})
	}
	if p != nil {
		ctx = identity.WithPrincipal(ctx, *p)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r.WithContext(ctx))
	return w
}

func TestServiceTokenRevokeIsOperatorLocalOnlyAndPrecise(t *testing.T) {
	h, is := serviceTokenHandler(t)
	issue := callServiceTokens(h, http.MethodPost, `{"app":"cerberus","audience":"tether","scopes":["events"]}`, &opP, true)
	var out ServiceTokenResponse
	if err := json.Unmarshal(issue.Body.Bytes(), &out); err != nil || issue.Code != 200 {
		t.Fatalf("%d %s", issue.Code, issue.Body)
	}
	body := `{"jti":"` + out.Record.JTI + `"}`
	device := identity.Principal{Verified: true, ID: "msg://device/d", Kind: "device", Scopes: []string{"admin", "maintain"}}
	service := identity.Principal{Verified: true, ID: out.Record.Sub, Kind: "service", Scopes: []string{"tether.events"}, TokenID: out.Record.JTI}
	for _, c := range []struct {
		name  string
		p     *identity.Principal
		local bool
		want  int
	}{{"anonymous", nil, true, 401}, {"remote operator", &opP, false, 403}, {"device admin", &device, true, 403}, {"the token itself", &service, true, 403}} {
		if w := callRevoke(h, body, c.p, c.local); w.Code != c.want {
			t.Errorf("%s: %d %s", c.name, w.Code, w.Body)
		}
	}
	if is.State.Denied("jti", out.Record.JTI) {
		t.Fatal("a refused request revoked the token")
	}
	for _, bad := range []string{`{}`, `{"jti":"a","app":"b"}`, `not json`} {
		if w := callRevoke(h, bad, &opP, true); w.Code != 400 {
			t.Errorf("%s: %d", bad, w.Code)
		}
	}
	if w := callRevoke(h, `{"jti":"nope"}`, &opP, true); w.Code != 404 {
		t.Errorf("unknown jti: %d", w.Code)
	}
	w := callRevoke(h, body, &opP, true)
	var rv ServiceTokenRevokeResponse
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &rv) != nil || len(rv.Revoked) != 1 || rv.Revoked[0].RevokedAt == nil || strings.Contains(w.Body.String(), out.Token.Token) {
		t.Fatalf("revoke: %d %s", w.Code, w.Body)
	}
	if !is.State.Denied("jti", out.Record.JTI) {
		t.Fatal("not on the deny-list")
	}
	if w := callRevoke(h, body, &opP, true); w.Code != 404 {
		t.Errorf("revoking twice: %d", w.Code)
	}
}

// An app that is NOT Tether verifies the service token with the go-auth
// library against Tether's JWKS and polls the public deny-list. After a
// revocation it stops accepting the token within the poll interval, with no
// credential used for the poll.
func TestExternalVerifierStopsAcceptingARevokedServiceTokenWithinThePollInterval(t *testing.T) {
	h, is := serviceTokenHandler(t)
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == DenyListPath && r.Method == http.MethodGet:
			NewDenyListReadHandler(is).ServeHTTP(w, r) // mounted public, as in the daemon
		default:
			h.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(hub.Close)
	issue := callServiceTokens(h, http.MethodPost, `{"app":"tachyon","audience":"tachyon","scopes":["tachyon.approvals_enqueue"]}`, &opP, true)
	var out ServiceTokenResponse
	if err := json.Unmarshal(issue.Body.Bytes(), &out); err != nil || issue.Code != 200 {
		t.Fatalf("%d %s", issue.Code, issue.Body)
	}
	deny := auth.NewDenyList()
	poller := &auth.DenyListPoller{URL: hub.URL + DenyListPath, List: deny, Interval: 100 * time.Millisecond} // no credential
	jv, err := auth.NewJWTVerifier(auth.JWTConfig{Issuer: "tether", Audience: "tachyon", JWKSURL: hub.URL + JWKSPath, DenyList: deny, MinRefresh: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go poller.Run(ctx)
	p, err := jv.Verify(context.Background(), out.Token.Token)
	if err != nil || p.Kind != "service" || p.ID != "msg://service/tachyon/dev" {
		t.Fatalf("before revoke: %+v %v", p, err)
	}
	if w := callRevoke(h, `{"jti":"`+out.Record.JTI+`"}`, &opP, true); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := jv.Verify(context.Background(), out.Token.Token); err != nil {
			return // refused within the poll interval
		}
		if time.Now().After(deadline) {
			t.Fatal("the revoked service token was still accepted 3 s (30x the poll interval) after revocation")
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func TestPublicDenyListReadHandler(t *testing.T) {
	_, is := serviceTokenHandler(t)
	_ = is.State.Deny(authissuer.DenySID, "gone", time.Now().Add(time.Hour))
	h := NewDenyListReadHandler(is)
	get := func(method, etag string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, DenyListPath, nil) // no credential at all
		if etag != "" {
			r.Header.Set("If-None-Match", etag)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	w := get(http.MethodGet, "")
	var doc struct{ JTI, Sub, SID []string }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &doc) != nil || len(doc.SID) != 1 || doc.SID[0] != "gone" || w.Header().Get("ETag") == "" {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if got := get(http.MethodGet, w.Header().Get("ETag")); got.Code != 304 {
		t.Fatalf("etag: %d", got.Code)
	}
	if got := get(http.MethodHead, ""); got.Code != 200 {
		t.Fatalf("head: %d", got.Code)
	}
	if got := get(http.MethodPost, ""); got.Code != http.StatusMethodNotAllowed {
		t.Fatalf("post: %d", got.Code)
	}
	// Only opaque ids: the document has exactly the three arrays.
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(w.Body.Bytes(), &raw)
	if len(raw) != 3 {
		t.Fatalf("deny-list document keys: %v", raw)
	}
}

func callRotate(h http.Handler, body string, p *identity.Principal, local bool) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, ServiceTokensPath+"/rotate-key", strings.NewReader(body))
	ctx := r.Context()
	if local {
		ctx = identity.ConnectionContext(ctx, &net.UnixConn{})
	}
	if p != nil {
		ctx = identity.WithPrincipal(ctx, *p)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r.WithContext(ctx))
	return w
}

func TestServiceKeyRotationAndRevokeAllAreOperatorLocalOnly(t *testing.T) {
	h, is := serviceTokenHandler(t)
	issue := func(app string) ServiceTokenResponse {
		w := callServiceTokens(h, http.MethodPost, `{"app":"`+app+`","audience":"tether","scopes":["events"]}`, &opP, true)
		var out ServiceTokenResponse
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
		return out
	}
	a, b := issue("cerberus"), issue("nanite")
	device := identity.Principal{Verified: true, ID: "msg://device/d", Kind: "device", Scopes: []string{"admin", "maintain"}}
	for _, c := range []struct {
		name  string
		p     *identity.Principal
		local bool
		want  int
	}{{"anonymous", nil, true, 401}, {"remote operator", &opP, false, 403}, {"device admin", &device, true, 403}} {
		if w := callRotate(h, `{}`, c.p, c.local); w.Code != c.want {
			t.Errorf("rotate %s: %d", c.name, w.Code)
		}
		if w := callRevoke(h, `{"all":true}`, c.p, c.local); w.Code != c.want {
			t.Errorf("revoke all %s: %d", c.name, w.Code)
		}
	}
	if is.State.Denied("jti", a.Record.JTI) {
		t.Fatal("a refused request revoked a token")
	}

	// Rotate without re-issue: reports what needs re-delivery, mints and prints nothing secret.
	w := callRotate(h, `{}`, &opP, true)
	var rot ServiceKeyRotateResponse
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &rot) != nil || !strings.HasPrefix(rot.NewKID, "svc-") || len(rot.Pending) != 2 || len(rot.Reissued) != 0 ||
		strings.Contains(w.Body.String(), a.Token.Token) {
		t.Fatalf("rotate: %d %s", w.Code, w.Body)
	}
	// Rotate with re-issue: replacement tokens appear once, in this response only.
	w = callRotate(h, `{"reissue":true}`, &opP, true)
	rot = ServiceKeyRotateResponse{}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &rot) != nil || len(rot.Reissued) != 2 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("reissue: %d %s", w.Code, w.Body)
	}
	if list := callServiceTokens(h, http.MethodGet, "", &opP, true); strings.Contains(list.Body.String(), rot.Reissued[0].Token.Token) {
		t.Fatal("the listing exposes a re-issued token")
	}

	// revoke: all/app are exclusive selectors.
	for _, bad := range []string{`{"all":true,"jti":"x"}`, `{"all":true,"env":"dev"}`, `{"all":true,"app":"Bad App"}`, `{"jti":"x","app":"y"}`, `{}`} {
		if w := callRevoke(h, bad, &opP, true); w.Code != 400 {
			t.Errorf("%s: %d", bad, w.Code)
		}
	}
	w = callRevoke(h, `{"all":true,"app":"cerberus"}`, &opP, true)
	var rv ServiceTokenRevokeResponse
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &rv) != nil || len(rv.Revoked) != 2 { // cerberus: the original and its re-issue
		t.Fatalf("cerberus revoke: %d %s", w.Code, w.Body)
	}
	if !is.State.Denied("jti", a.Record.JTI) || is.State.Denied("jti", b.Record.JTI) {
		t.Fatal("revoke --all --app cerberus did not stop exactly cerberus")
	}
	if w := callRevoke(h, `{"all":true}`, &opP, true); w.Code != 200 || !is.State.Denied("jti", b.Record.JTI) {
		t.Fatalf("revoke all: %d", w.Code)
	}
	if w := callRevoke(h, `{"all":true}`, &opP, true); w.Code != 404 {
		t.Fatalf("nothing left: %d", w.Code)
	}
}
