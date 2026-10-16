package api

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/store"
)

type fakeSessions map[string]*store.SessionRow

func (f fakeSessions) GetSession(id string) (*store.SessionRow, error) {
	if row, ok := f[id]; ok {
		return row, nil
	}
	return nil, sql.ErrNoRows
}

func TestCallerContextBindsOnlyToAnIssuedSessionBoundAgentToken(t *testing.T) {
	sessions := fakeSessions{"s1": {ID: "s1", LaunchID: "launch-1", ProjectID: "proj-1", LogicalAgentID: "la-1", WorkstreamID: sql.NullString{String: "ws-1", Valid: true}}}
	agent := identity.Principal{Verified: true, ID: identity.SessionPrincipalIDPrefix + "s1", Kind: "agent", SessionID: "s1", TokenID: "jti-1"}
	got := ResolveCallerContext(identity.WithPrincipal(context.Background(), agent), sessions, nil)
	if !got.SessionResolved || got.SessionID != "s1" || got.LaunchID != "launch-1" || got.ProjectID != "proj-1" || got.WorkstreamID != "ws-1" || got.PrincipalKind != "agent" {
		t.Fatalf("agent token not bound to its session: %+v", got)
	}
	for name, p := range map[string]identity.Principal{
		"operator-minted narrow": {Verified: true, ID: identity.OperatorID, Kind: "operator", TokenID: "j", Scopes: []string{"tether.read"}},
		"operator with a sid":    {Verified: true, ID: identity.OperatorID, Kind: "operator", TokenID: "j", SessionID: "s1"},
		"agent without token id": {Verified: true, ID: identity.SessionPrincipalIDPrefix + "s1", Kind: "agent", SessionID: "s1"},
		"agent, wrong subject":   {Verified: true, ID: identity.SessionPrincipalIDPrefix + "x", Kind: "agent", SessionID: "s1", TokenID: "j"},
	} {
		if s := ResolveCallerContext(identity.WithPrincipal(context.Background(), p), sessions, nil); s.SessionResolved || s.SessionID != "" {
			t.Errorf("%s resolved a session: %+v", name, s)
		}
	}
}

// In enforce mode an issued token is route-scoped. The MCP gateway has no
// scope (local custody), so only a session-bound agent token may reach it.
func TestAgentScopeMiddlewareLetsOnlyBoundAgentTokensReachTheGateway(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	h := AgentScopeMiddleware(identity.Enforce, ok)
	agent := identity.Principal{Verified: true, ID: identity.SessionPrincipalIDPrefix + "s1", Kind: "agent", SessionID: "s1", TokenID: "j", Scopes: []string{"tether.messages"}}
	narrow := identity.Principal{Verified: true, ID: identity.OperatorID, Kind: "operator", TokenID: "j", Scopes: []string{"tether.read"}}
	forged := identity.Principal{Verified: true, ID: "msg://device/d", Kind: "device", TokenID: "j", SessionID: "s1", Scopes: []string{"tether.read"}}
	call := func(p identity.Principal, method, path string) int {
		r := httptest.NewRequest(method, path, nil).WithContext(identity.WithPrincipal(context.Background(), p))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	for _, path := range []string{"/mcp", "/p/profile"} {
		if c := call(agent, "POST", path); c != http.StatusNoContent {
			t.Errorf("bound agent token refused at %s: %d", path, c)
		}
		if c := call(narrow, "POST", path); c == http.StatusNoContent {
			t.Errorf("operator-minted narrow token reached %s", path)
		}
		if c := call(forged, "POST", path); c == http.StatusNoContent {
			t.Errorf("device token carrying a sid reached %s", path)
		}
	}
	// a non-canonical path must not borrow the gateway exemption
	for _, path := range []string{"/p/../sessions/bootstrap", "/p/./../mcp/../sessions/bootstrap", "/mcp/../sessions/bootstrap", "//mcp", "/p/a/../../sessions/bootstrap"} {
		if c := call(agent, "POST", path); c == http.StatusNoContent {
			t.Errorf("bound agent token admitted at non-canonical %q", path)
		}
	}
	// the exemption is only the gateway: another local-custody route stays refused
	if c := call(agent, "POST", "/sessions/bootstrap"); c == http.StatusNoContent {
		t.Error("bound agent token reached a local-custody route")
	}
}
