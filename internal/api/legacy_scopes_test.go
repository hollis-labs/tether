package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hollis-labs/tether/internal/identity"
)

type tokenVerifier map[string]identity.Principal

func (v tokenVerifier) Verify(_ context.Context, token string) (identity.Principal, error) {
	if p, ok := v[token]; ok {
		return p, nil
	}
	return identity.Principal{}, identity.ErrInvalidToken
}

func legacyPrincipals() tokenVerifier {
	return tokenVerifier{
		"operator": {ID: identity.OperatorID, Kind: "operator", Scopes: []string{"*"}},
		"session": {ID: "msg://session/local/s1", Kind: "session", SessionID: "s1", Scopes: []string{"session.write", "message.write", "catalog.write"},
			Addresses: []string{"msg://session/local/s1", "msg://agent/agent-mux/la1"}},
		"device":  {ID: "msg://device/d", Kind: "device", Scopes: []string{"read", "operate"}},
		"service": {ID: "msg://service/x", Kind: "service", Scopes: []string{"read"}},
		// A non-session principal holding the legacy names gets no mapping.
		"service-legacy": {ID: "msg://service/y", Kind: "service", Scopes: []string{"session.write", "message.write", "catalog.write"}},
	}
}

// Route classes: what a session in normal work reaches, and what it never does.
var legacyRouteCases = []struct {
	class, method, path string
	session             bool // allowed for the minted session principal
	device, service     bool // allowed for device{read,operate} / service{read}
}{
	{"session read", "GET", "/sessions", true, true, true},
	{"session read", "GET", "/sessions/other/events", true, true, true},
	{"session read", "GET", "/sessions/s1/checkpoints", true, true, true},
	{"session read", "GET", "/whoami", true, true, true},
	{"session self operate", "POST", "/sessions/s1/checkpoint", true, true, false},
	{"session self operate", "POST", "/sessions/s1/refs", true, true, false},
	{"session self operate", "POST", "/sessions/s1/launch", true, true, false},
	{"session launch child", "POST", "/sessions", true, true, false},
	{"session other control", "POST", "/sessions/other/stop", false, true, false},
	{"session other control", "POST", "/sessions/other/turn", false, true, false},
	{"session other control", "POST", "/sessions/other/checkpoint", false, true, false},
	{"session terminal", "POST", "/sessions/s1/input", false, false, false},
	{"session terminal", "GET", "/sessions/s1/log", false, false, false},
	{"logical agent", "GET", "/logical-agents", true, true, true},
	{"logical agent self resume", "POST", "/logical-agents/la1/resume", true, true, false},
	{"logical agent other resume", "POST", "/logical-agents/la2/resume", false, true, false},
	{"logical agent policy write", "PATCH", "/logical-agents/la1/policy", false, false, false},
	{"messaging", "POST", "/messages", true, true, false},
	{"messaging", "GET", "/messages/inbox", true, true, true},
	{"messaging", "POST", "/messages/m1/reply", true, true, false},
	{"messaging", "POST", "/broker/envelopes", true, true, false},
	{"messaging maintain", "POST", "/messages/m1/purge", false, false, false},
	{"messaging maintain", "DELETE", "/messages/m1", false, false, false},
	{"groups", "POST", "/groups/g1/messages", true, true, false},
	{"channels", "GET", "/channels", true, true, true},
	{"workstreams", "POST", "/workstreams", true, true, false},
	{"catalog", "GET", "/catalog/projects", true, true, true},
	{"registry read", "GET", "/registry/agents", true, true, true},
	{"registry write", "POST", "/registry/agents", false, false, false},
	{"registry write", "POST", "/registry/bootstrap", false, false, false},
	{"settings write", "PUT", "/settings/mcp", false, false, false},
	{"identity admin", "GET", "/auth/devices", false, false, false},
	{"identity admin", "POST", "/auth/revoke", false, false, false},
	{"identity local", "POST", "/auth/pair", false, false, false},
	{"identity local", "POST", "/sessions/bootstrap", false, false, false},
	{"team formation", "POST", "/teams/form", false, true, false},
	{"team formation", "POST", "/teams/member_add", false, true, false},
	{"team roster", "GET", "/teams/roster", true, true, true},
	{"team result", "POST", "/teams/report_result", true, true, false},
	{"session groups", "POST", "/session-groups", false, true, false},
	{"ai chat", "POST", "/ai/chat", false, true, false},
	{"ai read", "GET", "/ai/models", true, true, true},
	{"public", "GET", "/health", true, true, true},
}

// Both modes run through the real middleware. Observe never rejects a
// request. Enforce rejects exactly what the mapping does not allow, and
// anonymous callers are refused with 401 (observe lets them through).
func TestLegacyScopeMappingByRouteClass(t *testing.T) {
	verifier := legacyPrincipals()
	run := func(mode identity.Mode, enforceScopes bool, token, method, path string) int {
		policy := identity.ScopePolicy{Observe: ObserveLocalScope, Enforce: enforceScopes}
		h := identity.MiddlewareWithScopes(mode, verifier, func(context.Context, identity.Observation) error { return nil }, policy,
			http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
		req := httptest.NewRequest(method, path, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	for _, tc := range legacyRouteCases {
		name := tc.class + " " + tc.method + " " + tc.path
		want := map[string]bool{"operator": tc.path != "", "session": tc.session, "device": tc.device, "service": tc.service, "service-legacy": false}
		if tc.path == "/health" {
			want["service-legacy"] = true
		}
		for token, allowed := range want {
			if got := run(identity.Observe, false, token, tc.method, tc.path); got != http.StatusNoContent {
				t.Errorf("observe %s as %s: rejected with %d", name, token, got)
			}
			if got := run(identity.Observe, true, token, tc.method, tc.path); (got == http.StatusNoContent) != allowed {
				t.Errorf("observe+scope-enforce %s as %s: status %d, allowed want %v", name, token, got, allowed)
			}
			if got := run(identity.Enforce, true, token, tc.method, tc.path); (got == http.StatusNoContent) != allowed {
				t.Errorf("enforce %s as %s: status %d, allowed want %v", name, token, got, allowed)
			}
		}
		if got := run(identity.Observe, false, "", tc.method, tc.path); got != http.StatusNoContent {
			t.Errorf("observe anonymous %s: %d", name, got)
		}
		wantAnon := http.StatusUnauthorized
		if tc.path == "/health" {
			wantAnon = http.StatusNoContent
		}
		if got := run(identity.Enforce, true, "", tc.method, tc.path); got != wantAnon {
			t.Errorf("enforce anonymous %s: %d want %d", name, got, wantAnon)
		}
	}
}

// Every mapped grant names a real table path, so a renamed route cannot leave
// a grant silently matching nothing.
func TestLegacyScopeGrantsReferenceTable(t *testing.T) {
	for held, grants := range LegacyScopeGrants {
		for _, g := range grants {
			found := false
			for _, rule := range LocalRouteScopes {
				if underPath(rule.Path, g.Path) {
					found = true
				}
			}
			if !found {
				t.Errorf("%s grant %s matches no table route", held, g.Path)
			}
			for _, s := range g.Scopes {
				if s != readScope && s != operateScope {
					t.Errorf("%s grant %s maps to %s; sessions only ever map to read/operate", held, g.Path, s)
				}
			}
		}
	}
}
