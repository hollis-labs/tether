package api

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
