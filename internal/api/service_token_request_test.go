package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/hollis-labs/tether/internal/authissuer"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/store"
)

type reqSessions map[string]*store.SessionRow

func (f reqSessions) GetSession(id string) (*store.SessionRow, error) {
	if r, ok := f[id]; ok {
		return r, nil
	}
	return nil, store.ErrSessionNotFound
}

type reqAudit struct {
	mu   sync.Mutex
	rows []identity.Observation
}

func (a *reqAudit) record(_ context.Context, o identity.Observation) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.rows = append(a.rows, o)
	return nil
}

func sessRow(id, app, state string) *store.SessionRow {
	r := &store.SessionRow{ID: id, State: state}
	if app != "" {
		r.App = sql.NullString{String: app, Valid: true}
	}
	return r
}

type requestFixture struct {
	is    *authissuer.Issuer
	audit *reqAudit
	do    func(who, body string, remote bool) (int, string)
}

// newRequestFixture serves the issuer handler over a real unix socket (so the
// connection counts as local) behind a test shim that sets the caller from a header.
func newRequestFixture(t *testing.T, mode string, sessions reqSessions) *requestFixture {
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
	audit := &reqAudit{}
	h := NewTokenIssuerHandler(is, WithMintAudit(audit.record), WithServiceRequests(sessions, mode))
	callers := map[string]identity.Principal{
		"s1":       {Verified: true, ID: "msg://session/local/s1", Kind: "session", SessionID: "s1", Scopes: []string{"session.write"}},
		"agent-s1": {Verified: true, ID: "msg://session/local/s1", Kind: "agent", SessionID: "s1", TokenID: "j1", Scopes: []string{"tether.read"}},
		"s2":       {Verified: true, ID: "msg://session/local/s2", Kind: "session", SessionID: "s2"},
		"s-none":   {Verified: true, ID: "msg://session/local/s-none", Kind: "session", SessionID: "s-none"},
		"s-ended":  {Verified: true, ID: "msg://session/local/s-ended", Kind: "session", SessionID: "s-ended"},
		"ghost":    {Verified: true, ID: "msg://session/local/ghost", Kind: "session", SessionID: "ghost"},
		"operator": {Verified: true, ID: identity.OperatorID, Kind: "operator", Scopes: []string{"*"}},
		"device":   {Verified: true, ID: "dev-1", Kind: "device", Scopes: []string{"tether.read"}},
		"service":  {Verified: true, ID: "msg://service/torque/dev", Kind: "service", TokenID: "j9"},
		"claimed":  {Verified: false, ID: "msg://session/local/s1", Kind: "session", SessionID: "s1"},
	}
	shim := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		if p, ok := callers[r.Header.Get("X-Test-Caller")]; ok {
			ctx = identity.WithPrincipal(ctx, p)
		}
		if r.Header.Get("X-Test-Remote") == "1" {
			ctx = identity.WithRemoteContext(ctx)
		}
		h.ServeHTTP(w, r.WithContext(ctx))
	})
	short, err := os.MkdirTemp("", "str")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(short) })
	sock := filepath.Join(short, "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: shim, ConnContext: identity.ConnectionContext}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}}
	return &requestFixture{is: is, audit: audit, do: func(who, body string, remote bool) (int, string) {
		req, _ := http.NewRequest("POST", "http://tether"+ServiceTokenRequestPath, strings.NewReader(body))
		req.Header.Set("X-Test-Caller", who)
		if remote {
			req.Header.Set("X-Test-Remote", "1")
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}}
}

func defaultSessions() reqSessions {
	return reqSessions{
		"s1":      sessRow("s1", "torque", "running"),
		"s2":      sessRow("s2", "torque", "running"),
		"s-none":  sessRow("s-none", "", "running"),
		"s-ended": func() *store.SessionRow { r := sessRow("s-ended", "torque", "completed"); return r }(),
	}
}

func TestSessionRequestsItsOwnAppsTokenInAutoMode(t *testing.T) {
	f := newRequestFixture(t, ServiceRequestAuto, defaultSessions())
	for _, who := range []string{"s1", "agent-s1"} {
		code, body := f.do(who, `{"label":"torque emitter"}`, false)
		var out struct {
			Token  string `json:"token"`
			Record struct {
				JTI, Sub, Aud string
				Scopes        []string
			} `json:"record"`
		}
		_ = json.Unmarshal([]byte(body), &out)
		if code != http.StatusOK || out.Token == "" || out.Record.Sub != "msg://service/torque/dev" {
			t.Fatalf("%s: %d %.200s", who, code, body)
		}
		// The audit row names requester, session, app, decision and a jti prefix, never the token.
		last := f.audit.rows[len(f.audit.rows)-1]
		if !strings.Contains(last.Event, "app=torque") || !strings.Contains(last.Event, "decision=issued") || !strings.Contains(last.Event, "jti="+out.Record.JTI[:8]) || last.SessionID != "s1" {
			t.Fatalf("audit row %+v", last)
		}
		if strings.Contains(last.Event, out.Token) || strings.Contains(last.RequiredScope, out.Token) || strings.Contains(last.PrincipalID, out.Token) {
			t.Fatal("the audit row holds the token")
		}
	}
}

func TestRequestIsApprovalByDefaultAndIssuesNothing(t *testing.T) {
	for _, mode := range []string{"", "approval", "AUTO ", "yes"} {
		f := newRequestFixture(t, mode, defaultSessions())
		code, body := f.do("s1", `{}`, false)
		if code != http.StatusAccepted || strings.Contains(body, "eyJ") {
			t.Fatalf("mode %q: %d %s", mode, code, body)
		}
		if n := len(f.is.State.Services()); n != 0 {
			t.Fatalf("mode %q minted %d tokens", mode, n)
		}
		if last := f.audit.rows[len(f.audit.rows)-1]; !strings.Contains(last.Event, "decision=pending") {
			t.Fatalf("mode %q audit %+v", mode, last)
		}
	}
	// Approval mode still refuses what auto mode would refuse, before queueing it.
	f := newRequestFixture(t, "", defaultSessions())
	if code, _ := f.do("s-none", `{}`, false); code != http.StatusForbidden {
		t.Fatalf("no-app session in approval mode: %d", code)
	}
	if code, _ := f.do("s1", `{"scopes":["tether.read"]}`, false); code != http.StatusForbidden {
		t.Fatalf("scope outside the allowlist in approval mode: %d", code)
	}
}

func TestRequestRefusals(t *testing.T) {
	f := newRequestFixture(t, ServiceRequestAuto, defaultSessions())
	cases := []struct {
		name, who, body string
		remote          bool
		want            int
	}{
		{"session with no app", "s-none", `{}`, false, http.StatusForbidden},
		{"ended session", "s-ended", `{}`, false, http.StatusForbidden},
		{"session unknown to the hub", "ghost", `{}`, false, http.StatusForbidden},
		{"body names an app", "s1", `{"app":"cerberus"}`, false, http.StatusBadRequest},
		{"body names an env", "s1", `{"env":"prod"}`, false, http.StatusBadRequest},
		{"body names an audience", "s1", `{"audience":"torque"}`, false, http.StatusBadRequest},
		{"body names a session id", "s1", `{"session_id":"s2"}`, false, http.StatusBadRequest},
		{"scope outside the allowlist", "s1", `{"scopes":["tether.read"]}`, false, http.StatusForbidden},
		{"wildcard scope", "s1", `{"scopes":["tether.*"]}`, false, http.StatusBadRequest},
		{"another app's scope", "s1", `{"scopes":["torque.approve"]}`, false, http.StatusBadRequest},
		{"operator", "operator", `{}`, false, http.StatusForbidden},
		{"device", "device", `{}`, false, http.StatusForbidden},
		{"service", "service", `{}`, false, http.StatusForbidden},
		{"unverified claim", "claimed", `{}`, false, http.StatusUnauthorized},
		{"no caller", "", `{}`, false, http.StatusUnauthorized},
		{"remote caller with a session identity", "s1", `{}`, true, http.StatusForbidden},
	}
	for _, tc := range cases {
		code, body := f.do(tc.who, tc.body, tc.remote)
		if code != tc.want || strings.Contains(body, "eyJ") {
			t.Errorf("%s: %d %.160s, want %d", tc.name, code, body, tc.want)
		}
	}
	if n := len(f.is.State.Services()); n != 0 {
		t.Fatalf("refused requests minted %d tokens", n)
	}
}

func TestRequestCapIsPerSession(t *testing.T) {
	f := newRequestFixture(t, ServiceRequestAuto, defaultSessions())
	for i := 0; i < 5; i++ {
		if code, body := f.do("s1", `{}`, false); code != http.StatusOK {
			t.Fatalf("request %d: %d %s", i, code, body)
		}
	}
	if code, _ := f.do("s1", `{}`, false); code != http.StatusForbidden {
		t.Fatalf("sixth request: %d", code)
	}
	if code, _ := f.do("s2", `{}`, false); code != http.StatusOK {
		t.Fatalf("another session of the same app: %d", code)
	}
}

func TestRequestRouteIsLocalOnlyInTheRemoteTable(t *testing.T) {
	for _, tbl := range [][]RouteScope{RemoteRouteScopes, LocalRouteScopes} {
		var found bool
		for _, r := range tbl {
			if r.Path == ServiceTokenRequestPath {
				found = r.Methods["POST"] == localScope
			}
		}
		if !found {
			t.Fatal("the request route is missing from a scope table or is not local-only")
		}
	}
}

// Every refusal of a verified caller leaves one audit row, including a malformed or
// unknown-field body, and no row ever carries request content. Unverified and anonymous
// callers leave none (identity/guard.go: unauthenticated traffic allocates no audit rows).
func TestRequestAuditPolicy(t *testing.T) {
	f := newRequestFixture(t, ServiceRequestAuto, defaultSessions())
	rows := func() int {
		f.audit.mu.Lock()
		defer f.audit.mu.Unlock()
		return len(f.audit.rows)
	}
	for name, body := range map[string]string{
		"malformed json":      `{"scopes": [`,
		"unknown field":       `{"app":"SECRET-APP-VALUE"}`,
		"trailing document":   `{} {"x":"SECRET-TRAILER"}`,
		"unknown field value": `{"label":"ok","token":"SECRET-TOKEN-VALUE"}`,
	} {
		before := rows()
		if code, _ := f.do("s1", body, false); code != http.StatusBadRequest {
			t.Fatalf("%s: %d", name, code)
		}
		if rows() != before+1 {
			t.Fatalf("%s: %d audit rows added, want 1", name, rows()-before)
		}
		last := f.audit.rows[len(f.audit.rows)-1]
		if !strings.Contains(last.Event, "decision=refused_invalid") || last.SessionID != "s1" || last.PrincipalID != "msg://session/local/s1" {
			t.Fatalf("%s: audit row %+v", name, last)
		}
		for _, secret := range []string{"SECRET-APP-VALUE", "SECRET-TRAILER", "SECRET-TOKEN-VALUE"} {
			if strings.Contains(last.Event, secret) || strings.Contains(last.RequiredScope, secret) {
				t.Fatalf("%s: the audit row holds request content", name)
			}
		}
	}
	// Refused verified callers (wrong kind) are audited; unverified and anonymous are not.
	before := rows()
	f.do("operator", `{}`, false)
	if rows() != before+1 {
		t.Fatalf("a refused verified operator left %d rows", rows()-before)
	}
	before = rows()
	f.do("claimed", `{}`, false)
	f.do("", `{}`, false)
	f.do("claimed", `{"bad":`, false)
	if rows() != before {
		t.Fatalf("unverified or anonymous callers left %d audit rows", rows()-before)
	}
}
