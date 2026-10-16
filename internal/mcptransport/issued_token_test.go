package mcptransport

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/authissuer"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/mcpgateway"
	"github.com/hollis-labs/tether/internal/store"
)

// issuedRig serves /mcp with the hub's real verifier (stored credentials plus issued JWTs) and mints aud=tether
// agent tokens for sessions the way the outpost's token minter does.
type issuedRig struct {
	f      *transportFixture
	issuer *authissuer.Issuer
}

func newIssuedRig(t *testing.T) *issuedRig {
	t.Helper()
	root := t.TempDir()
	keys, err := authissuer.OpenKeyStore(filepath.Join(root, "auth"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	state, err := authissuer.OpenState(filepath.Join(root, "auth", "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	issuer := authissuer.New("tether-test", keys, state)
	f := newTransportFixtureWithIssuer(t, false, false, time.Minute, issuer)
	for _, sid := range []string{"s-rot", "s-other"} {
		if err := f.db.CreateSession(store.SessionRow{ID: sid, LaunchID: "l", ProjectID: "p", LogicalAgentID: "agent", ProviderID: "x", ProviderKind: "cli", Workspace: "/w", State: "running"}, nil); err != nil {
			t.Fatal(err)
		}
		// The hub serves a bound agent token with the session's saved MCP policy.
		if err := f.db.SaveSessionMCPPolicy(context.Background(), mcpgateway.SessionPolicy{SessionID: sid, AgentID: "agent", Servers: []string{}}.Seal()); err != nil {
			t.Fatal(err)
		}
	}
	return &issuedRig{f: f, issuer: issuer}
}

func (g *issuedRig) mint(t *testing.T, aud, sid string, scopes ...string) (token, jti string) {
	t.Helper()
	op := identity.Principal{Verified: true, ID: identity.OperatorID, Kind: "operator", Scopes: []string{"*"}}
	tok, err := g.issuer.Exchange(op, authissuer.Request{Audience: aud, Scopes: scopes, SessionID: sid})
	if err != nil {
		t.Fatal(err)
	}
	return tok.Token, tok.JTI
}

func (g *issuedRig) call(t *testing.T, token, session string) (int, string) {
	t.Helper()
	resp := g.f.request(t, http.MethodPost, "/mcp", token, session, nil)
	defer closeResponse(resp)
	return resp.StatusCode, resp.Header.Get("Mcp-Session-Id")
}

// CW-20261016-0160 (architect ruling mcp_session_fingerprint_issued_tokens_2026_10_16): an aud=tether agent token
// is admitted by /mcp, and a refresh of the same authority continues the open MCP session. The outpost rewrites
// the token every few minutes, so pinning the raw token would end every session at its first rotation.
func TestTransportIssuedTokenAdmittedAndRefreshContinuesSession(t *testing.T) {
	g := newIssuedRig(t)
	first, _ := g.mint(t, authissuer.TetherAudience, "s-rot", "tether.read")
	second, _ := g.mint(t, authissuer.TetherAudience, "s-rot", "tether.read")
	if first == second {
		t.Fatal("two mints produced the same token")
	}
	status, id := g.call(t, first, "")
	if status != http.StatusOK || id == "" {
		t.Fatalf("initialize with an aud=tether agent token (%d chars): status %d session %q", len(first), status, id)
	}
	if status, _ := g.call(t, first, id); status != http.StatusOK {
		t.Fatalf("same token on the session: %d", status)
	}
	if status, _ := g.call(t, second, id); status != http.StatusOK {
		t.Fatalf("refreshed token (same claims, new jti) on the open session: %d, want 200", status)
	}
}

// Any change of authority on the same MCP session is refused, narrowing included.
func TestTransportIssuedTokenAuthorityChangeRefused(t *testing.T) {
	g := newIssuedRig(t)
	wide, _ := g.mint(t, authissuer.TetherAudience, "s-rot", "tether.read", "tether.operate")
	_, id := g.call(t, wide, "")
	if id == "" {
		t.Fatal("no session")
	}
	for name, token := range map[string]string{
		"narrowed scopes": mustMint(t, g, authissuer.TetherAudience, "s-rot", "tether.read"),
		"widened scopes":  mustMint(t, g, authissuer.TetherAudience, "s-rot", "tether.read", "tether.operate", "tether.maintain"),
		"other session":   mustMint(t, g, authissuer.TetherAudience, "s-other", "tether.read", "tether.operate"),
	} {
		if status, _ := g.call(t, token, id); status != http.StatusForbidden {
			t.Errorf("%s: status %d, want 403", name, status)
		}
	}
	// A token for another audience never verifies for /mcp at all.
	if other, _ := g.mint(t, "tesseract", "s-rot", "tesseract.read"); true {
		if status, _ := g.call(t, other, id); status == http.StatusOK {
			t.Errorf("another audience's token was admitted")
		}
	}
	// The original authority is still welcome.
	again, _ := g.mint(t, authissuer.TetherAudience, "s-rot", "tether.read", "tether.operate")
	if status, _ := g.call(t, again, id); status != http.StatusOK {
		t.Errorf("identical authority refused: %d", status)
	}
}

func mustMint(t *testing.T, g *issuedRig, aud, sid string, scopes ...string) string {
	t.Helper()
	token, _ := g.mint(t, aud, sid, scopes...)
	return token
}

// The fingerprint never replaces per-request verification: a denied jti and an expired token are refused on an
// open session even though their claims match.
func TestTransportIssuedTokenRevokedOrExpiredRefusedOnOpenSession(t *testing.T) {
	g := newIssuedRig(t)
	live, _ := g.mint(t, authissuer.TetherAudience, "s-rot", "tether.read")
	_, id := g.call(t, live, "")
	if id == "" {
		t.Fatal("no session")
	}
	revoked, jti := g.mint(t, authissuer.TetherAudience, "s-rot", "tether.read")
	if status, _ := g.call(t, revoked, id); status != http.StatusOK {
		t.Fatalf("matching refresh before revocation: %d", status)
	}
	if err := g.issuer.State.Deny(authissuer.DenyJTI, jti, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	status := http.StatusOK
	for time.Now().Before(deadline) { // the deny-list is polled; allow it to land
		if status, _ = g.call(t, revoked, id); status != http.StatusOK {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if status != http.StatusUnauthorized {
		t.Errorf("revoked jti on an open session: %d, want 401", status)
	}

	g.issuer.Now = func() time.Time { return time.Now().Add(-time.Hour) }
	expired, _ := g.mint(t, authissuer.TetherAudience, "s-rot", "tether.read")
	g.issuer.Now = time.Now
	if status, _ := g.call(t, expired, id); status != http.StatusUnauthorized {
		t.Errorf("expired token with matching claims: %d, want 401", status)
	}
}

// CW-20261016-0160 A1 (architect notes 2 and 3): the view adopts a new token only after that request verified and
// matched its fingerprint, and never one that expires earlier than the pinned token. After the token the view was
// prepared with has really expired (the fixture's verifier allows 100ms of skew), the view keeps working on the
// adopted token, and revoking the adopted token's jti ends the view on the next request.
func TestTransportViewSurvivesFirstTokenExpiryAndDiesWithTheAdoptedJTI(t *testing.T) {
	g := newIssuedRig(t)
	op := identity.Principal{Verified: true, ID: identity.OperatorID, Kind: "operator", Scopes: []string{"*"}}
	short, err := g.issuer.Exchange(op, authissuer.Request{Audience: authissuer.TetherAudience, Scopes: []string{"tether.read"}, SessionID: "s-rot", TTL: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	_, id := g.call(t, short.Token, "")
	if id == "" {
		t.Fatal("no session")
	}
	// Keep refreshing across the first token's expiry.
	var adopted, adoptedJTI string
	deadline := time.Now().Add(2500 * time.Millisecond)
	for time.Now().Before(deadline) {
		adopted, adoptedJTI = g.mint(t, authissuer.TetherAudience, "s-rot", "tether.read")
		if status, _ := g.call(t, adopted, id); status != http.StatusOK {
			t.Fatalf("refreshed token on the open session: %d", status)
		}
		time.Sleep(250 * time.Millisecond)
	}
	// The pinned token is now really expired: it is refused on its own...
	if status, _ := g.call(t, short.Token, id); status != http.StatusUnauthorized {
		t.Fatalf("the expired first token: %d, want 401", status)
	}
	// ...but the view lives on, following the newest verified token.
	h := g.f.handler
	h.mu.Lock()
	v := h.sessions[id]
	h.mu.Unlock()
	if v == nil {
		t.Fatal("the view was dropped when its first token expired")
	}
	if got := h.viewToken(v); got != adopted {
		t.Fatal("the view is not following the latest verified token")
	}
	// A refresh that expires earlier than the pinned token never replaces it.
	older, err := g.issuer.Exchange(op, authissuer.Request{Audience: authissuer.TetherAudience, Scopes: []string{"tether.read"}, SessionID: "s-rot", TTL: 5 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if status, _ := g.call(t, older.Token, id); status != http.StatusOK {
		t.Fatalf("shorter-lived valid token: %d", status)
	}
	if got := h.viewToken(v); got != adopted {
		t.Fatal("a token that expires sooner replaced the view's current token")
	}
	// Revoking the adopted token's jti ends the view: the token itself is refused on its next request, and the
	// view, which holds that token, is removed by the monitor.
	if err := g.issuer.State.Deny(authissuer.DenyJTI, adoptedJTI, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if status, _ := g.call(t, adopted, id); status != http.StatusUnauthorized {
		t.Fatalf("the revoked adopted token: %d, want 401", status)
	}
	end := time.Now().Add(3 * time.Second)
	for time.Now().Before(end) {
		h.mu.Lock()
		gone := h.sessions[id] == nil
		h.mu.Unlock()
		if gone {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	fresh, _ := g.mint(t, authissuer.TetherAudience, "s-rot", "tether.read")
	if status, _ := g.call(t, fresh, id); status == http.StatusOK {
		t.Fatal("the view survived the revocation of its adopted token's jti")
	}
}
