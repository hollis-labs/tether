package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/authissuer"
	"github.com/hollis-labs/tether/internal/environment"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/store"
)

type remoteMCPRig struct {
	s       *Server
	db      *store.Store
	issuer  *authissuer.Issuer
	reached atomic.Int32
	remote  atomic.Bool
}

func newRemoteMCPRig(t *testing.T) *remoteMCPRig {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	dir := filepath.Join(t.TempDir(), "auth")
	keys, err := authissuer.OpenKeyStore(dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	state, err := authissuer.OpenState(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	g := &remoteMCPRig{db: db, issuer: authissuer.New("tether", keys, state)}
	for _, sid := range []string{"s-live", "s-ended"} {
		row := store.SessionRow{ID: sid, LaunchID: "l", ProjectID: "p", ProviderID: "x", ProviderKind: "cli", Workspace: "/w", State: "running", WorkerID: "worker-1"}
		if err := db.CreateSession(row, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.UpdateSessionState("s-ended", "completed", 0, nil); err != nil {
		t.Fatal(err)
	}
	mcp := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.reached.Add(1)
		g.remote.Store(identity.IsRemote(r.Context()))
		w.WriteHeader(http.StatusAccepted)
	})
	g.s = &Server{
		Config:   Config{IdentityMode: identity.Enforce, RemoteListener: RemoteListenerConfig{Enabled: true, ListenAddr: "tcp:127.0.0.1:7331", AllowedHosts: []string{"localhost:9000"}}},
		Identity: identity.NewStore(db.DB()), Issuer: g.issuer, MCP: mcp, RemoteMCPSessions: db,
	}
	t.Cleanup(g.s.CloseIdentityAudit)
	return g
}

func (g *remoteMCPRig) agentToken(t *testing.T, sid string) string {
	t.Helper()
	op := identity.Principal{Verified: true, ID: identity.OperatorID, Kind: "operator", Scopes: []string{"*"}}
	tok, err := g.issuer.Exchange(op, authissuer.Request{Audience: authissuer.TetherAudience, Scopes: []string{"tether.read"}, SessionID: sid})
	if err != nil {
		t.Fatal(err)
	}
	return tok.Token
}

func (g *remoteMCPRig) do(method, path, host, token, session string, extra map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://"+host+path, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if session != "" {
		r.Header.Set("Mcp-Session-Id", session)
	}
	if !strings.Contains(path, "/mcp") || token == "" || !strings.Contains(token, ".") {
		// The existing remote chain is Tether-client traffic and needs the protocol header; the MCP gate does not.
		r.Header.Set(environment.ProtocolHeader, "1")
	}
	for k, v := range extra {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	g.s.RemoteHandler("127.0.0.1:7331").ServeHTTP(w, r)
	return w
}

// CW-20261016-0160 H1 (architect ruling remote_mcp_bound_agent_tokens_2026_10_16): the remote listener serves /mcp
// to a bound issuer agent token for a live session, and nothing else changes. Exact status codes per case.
func TestRemoteMCPBoundAgentTokenOnly(t *testing.T) {
	g := newRemoteMCPRig(t)
	const host = "localhost:9000"
	expires := time.Now().Add(time.Hour)
	device, err := g.s.Identity.Mint(context.Background(), identity.Principal{ID: "remote-reader", Kind: "device", Scopes: []string{"read"}, ExpiresAt: &expires})
	if err != nil {
		t.Fatal(err)
	}
	live := g.agentToken(t, "s-live")
	op := identity.Principal{Verified: true, ID: identity.OperatorID, Kind: "operator", Scopes: []string{"*"}}
	nonAgent, err := g.issuer.Exchange(op, authissuer.Request{Audience: authissuer.TetherAudience, Scopes: []string{"tether.read"}})
	if err != nil {
		t.Fatal(err)
	}
	otherAud, err := g.issuer.Exchange(op, authissuer.Request{Audience: "tesseract", Scopes: []string{"tesseract.read"}, SessionID: "s-live"})
	if err != nil {
		t.Fatal(err)
	}
	denied, err := g.issuer.Exchange(op, authissuer.Request{Audience: authissuer.TetherAudience, Scopes: []string{"tether.read"}, SessionID: "s-live"})
	if err != nil {
		t.Fatal(err)
	}
	if err := g.issuer.State.Deny(authissuer.DenyJTI, denied.JTI, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	g.issuer.Now = func() time.Time { return time.Now().Add(-time.Hour) }
	expired := g.agentToken(t, "s-live")
	g.issuer.Now = time.Now
	tampered := live[:len(live)-4] + "AAAA"

	for _, tc := range []struct {
		name, method, path, host, token string
		want                            int
		reaches                         bool
	}{
		{"bound agent token, live session: the gateway", "POST", "/mcp", host, live, 202, true},
		{"(a) device credential: unchanged 404", "POST", "/mcp", host, device, 404, false},
		{"no credential: unchanged 401", "POST", "/mcp", host, "", 401, false},
		{"garbage non-JWT credential: unchanged 401", "POST", "/mcp", host, "garbage", 401, false},
		{"(b) tampered signature: 401", "POST", "/mcp", host, tampered, 401, false},
		{"(b) expired token: 401", "POST", "/mcp", host, expired, 401, false},
		{"(b) denied jti: 401", "POST", "/mcp", host, denied.Token, 401, false},
		{"(c) aud other than tether: 401", "POST", "/mcp", host, otherAud.Token, 401, false},
		{"(d) valid issued token that is not a bound agent: 404", "POST", "/mcp", host, nonAgent.Token, 404, false},
		{"(e) agent token on another remote route: unchanged 401", "GET", "/v1/environment/report", host, live, 401, false},
		{"(e) agent token on /p/: not served, unchanged 401", "POST", "/p/readonly", host, live, 401, false},
		{"(e) agent token on a near-miss path /mcp/: unchanged 401", "POST", "/mcp/", host, live, 401, false},
		{"(f) session already ended: 401", "POST", "/mcp", host, g.agentToken(t, "s-ended"), 401, false},
		{"(g) Host outside the allow-list: 403", "POST", "/mcp", "evil.example.com", live, 403, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := g.reached.Load()
			w := g.do(tc.method, tc.path, tc.host, tc.token, "", nil)
			if w.Code != tc.want {
				t.Fatalf("status %d, want %d: %s", w.Code, tc.want, w.Body.String())
			}
			if reached := g.reached.Load() != before; reached != tc.reaches {
				t.Fatalf("reached the MCP gateway = %v, want %v", reached, tc.reaches)
			}
		})
	}
	if !g.remote.Load() {
		t.Fatal("the gateway did not see server-owned remote provenance")
	}
}

// The gate counts its refusals by reason and audits each remote initialize with the worker and the Serve identity.
func TestRemoteMCPAuditAndDenialCounters(t *testing.T) {
	g := newRemoteMCPRig(t)
	live := g.agentToken(t, "s-live")
	if w := g.do("POST", "/mcp", "localhost:9000", live, "", map[string]string{"Tailscale-User-Login": "worker-1@tailnet.example"}); w.Code != 202 {
		t.Fatalf("initialize: %d", w.Code)
	}
	// A request on an existing MCP session is not an initialize: no second row.
	if w := g.do("POST", "/mcp", "localhost:9000", live, "sess-1", nil); w.Code != 202 {
		t.Fatalf("continued session: %d", w.Code)
	}
	g.do("POST", "/mcp", "localhost:9000", live[:len(live)-4]+"AAAA", "", nil)
	g.do("POST", "/mcp", "localhost:9000", g.agentToken(t, "s-ended"), "", nil)
	denials := g.s.RemoteMCPDenials()
	if denials["invalid_token"] != 1 || denials["session_ended"] != 1 || len(denials) != 2 {
		t.Fatalf("denials = %v", denials)
	}
	g.s.CloseIdentityAudit()
	rows, err := g.db.DB().Query(`SELECT principal_id, session_id, event, route FROM identity_audit WHERE event LIKE 'remote_mcp_initialize%'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var principal, sid, event, route string
		if err := rows.Scan(&principal, &sid, &event, &route); err != nil {
			t.Fatal(err)
		}
		n++
		if principal != "msg://session/local/s-live" || sid != "s-live" || route != "/mcp" || event != "remote_mcp_initialize worker=worker-1 node=worker-1@tailnet.example" {
			t.Fatalf("audit row = %q %q %q %q", principal, sid, route, event)
		}
	}
	if n != 1 {
		t.Fatalf("remote initialize audit rows = %d, want exactly 1", n)
	}
}

// The Tether-Protocol exemption is exactly POST/GET/DELETE on exactly /mcp, for a bound agent token. Anything else still
// takes the ordinary remote chain and still needs the header (architect ask on #392).
func TestRemoteMCPProtocolExemptionIsExact(t *testing.T) {
	g := newRemoteMCPRig(t)
	const host = "localhost:9000"
	live := g.agentToken(t, "s-live")
	expires := time.Now().Add(time.Hour)
	device, err := g.s.Identity.Mint(context.Background(), identity.Principal{ID: "remote-reader", Kind: "device", Scopes: []string{"read"}, ExpiresAt: &expires})
	if err != nil {
		t.Fatal(err)
	}
	send := func(method, path, token string, header bool) int {
		r := httptest.NewRequest(method, "http://"+host+path, strings.NewReader(`{}`))
		r.Header.Set("Authorization", "Bearer "+token)
		if header {
			r.Header.Set(environment.ProtocolHeader, "1")
		}
		w := httptest.NewRecorder()
		g.s.RemoteHandler("127.0.0.1:7331").ServeHTTP(w, r)
		return w.Code
	}
	// The gate serves exactly the MCP methods on exactly /mcp, with no protocol header.
	for _, method := range []string{http.MethodPost, http.MethodGet, http.MethodDelete} {
		before := g.reached.Load()
		if code := send(method, "/mcp", live, false); code != 202 || g.reached.Load() != before+1 {
			t.Errorf("%s /mcp without the header: %d (reached %d)", method, code, g.reached.Load()-before)
		}
	}
	// Other methods on /mcp are not the gate's: they take the ordinary chain and never reach the gateway.
	before := g.reached.Load()
	for _, method := range []string{http.MethodPut, http.MethodPatch, http.MethodHead, http.MethodOptions} {
		if code := send(method, "/mcp", live, false); code != 401 {
			t.Errorf("%s /mcp: %d, want the ordinary chain's 401", method, code)
		}
	}
	if g.reached.Load() != before {
		t.Error("a non-MCP method reached the gateway")
	}
	// Any other path under /mcp still needs the header: a valid device credential gets the protocol error without it
	// and the route's 404 with it. The gate is not involved for either.
	for _, path := range []string{"/mcp/anything", "/mcp/", "/mcpx"} {
		if code := send(http.MethodPost, path, device, false); code != 409 {
			t.Errorf("%s without the header: %d, want 409 protocol_mismatch", path, code)
		}
		if code := send(http.MethodPost, path, device, true); code != 404 {
			t.Errorf("%s with the header: %d, want 404", path, code)
		}
		if code := send(http.MethodPost, path, live, false); code == 202 {
			t.Errorf("%s reached the gateway with an agent token", path)
		}
	}
}

// A daemon with a remote listener but no MCP handler (daemon.mcp_endpoint off) must say so, not just refuse callers: the
// canary of 2026-10-16 read as a token problem for lack of this.
func TestRemoteMCPStatusReportsWhyTheGateIsMissing(t *testing.T) {
	g := newRemoteMCPRig(t)
	if st := g.s.RemoteMCPStatus(); st == nil || !st.Available || st.Reason != "" {
		t.Fatalf("with an MCP handler: %+v", st)
	}
	noMCP := newRemoteMCPRig(t)
	noMCP.s.MCP = nil
	st := noMCP.s.RemoteMCPStatus()
	if st == nil || st.Available || !strings.Contains(st.Reason, "daemon.mcp_endpoint.enabled") {
		t.Fatalf("without an MCP handler: %+v", st)
	}
	// The request is refused, not served, and no denial counter moves: there is no gate to count.
	w := noMCP.do("POST", "/mcp", "localhost:9000", noMCP.agentToken(t, "s-live"), "", nil)
	if w.Code != 401 || noMCP.reached.Load() != 0 || len(noMCP.s.RemoteMCPDenials()) != 0 {
		t.Fatalf("no-gate request: status %d reached %d denials %v", w.Code, noMCP.reached.Load(), noMCP.s.RemoteMCPDenials())
	}
	var none Server
	if none.RemoteMCPStatus() != nil {
		t.Fatal("status without a remote listener must be absent")
	}
}
