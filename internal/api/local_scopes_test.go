package api

import (
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/hollis-labs/tether/internal/identity"
)

// A new registration must get an explicit local policy; this fails otherwise.
func TestLocalScopesCoverRegisteredRoutes(t *testing.T) {
	policy := map[string]bool{}
	for _, rule := range LocalRouteScopes {
		policy[rule.Registration] = true
	}
	for _, pattern := range registeredRoutePatterns(t) {
		if !policy[pattern] {
			t.Errorf("registered route %s lacks explicit local policy", pattern)
		}
	}
	if _, _, known := RequiredLocalScope(httptest.NewRequest("GET", "/future/unclassified", nil)); known {
		t.Fatal("unknown route classified")
	}
}

// The local and remote tables are one shape and, until a deliberate local
// divergence is made, one content.
func TestLocalScopesShareRemoteTable(t *testing.T) {
	if !reflect.DeepEqual(LocalRouteScopes, RemoteRouteScopes) {
		t.Fatal("local table diverged from the remote table; record the divergence deliberately")
	}
	if &LocalRouteScopes[0] == &RemoteRouteScopes[0] {
		t.Fatal("local table aliases the remote table")
	}
}

func TestObserveLocalScope(t *testing.T) {
	operator := identity.Principal{ID: identity.OperatorID, Kind: "operator", Scopes: []string{"*"}}
	device := identity.Principal{ID: "msg://device/x", Kind: "device", Scopes: []string{"read"}}
	session := identity.Principal{ID: "msg://session/local/s", Kind: "session", Scopes: []string{"message.write"}}
	for _, tc := range []struct {
		method, path string
		p            identity.Principal
		pattern      string
		required     string
		allow, known bool
	}{
		{"GET", "/sessions/abc/events", operator, "/sessions/{id}/events", "read", true, true},
		{"GET", "/sessions/abc/events", device, "/sessions/{id}/events", "read", true, true},
		{"POST", "/sessions/abc/input", device, "/sessions/{id}/input", "input", false, true},
		{"POST", "/messages", session, "/messages", "messages", false, true},
		{"POST", "/auth/pair", device, "/auth/pair", "local-only", false, true},
		{"POST", "/auth/pair", operator, "/auth/pair", "local-only", true, true},
		{"GET", "/health", session, "/health", "public", true, true},
		{"DELETE", "/sessions", operator, "", "", false, false},
		{"GET", "/nope", operator, "", "", false, false},
	} {
		got := ObserveLocalScope(httptest.NewRequest(tc.method, tc.path, nil), tc.p)
		want := identity.ScopeObservation{Pattern: tc.pattern, Required: tc.required, WouldAllow: tc.allow, Known: tc.known}
		if got != want {
			t.Errorf("%s %s %s: got %+v want %+v", tc.method, tc.path, tc.p.Kind, got, want)
		}
	}
}
