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

// A registry-only service principal (CW-20261024-0094: Nanite's enrollment credential)
// can administer and read the registry but cannot send, read or list mail, read
// sessions or workers, or launch anything. tether.read keeps reading the registry.
func TestRegistryOnlyServicePrincipalIsLimitedToTheRegistry(t *testing.T) {
	registryOnly := identity.Principal{Verified: true, ID: "msg://service/nanite-registry/dev", Kind: "service", TokenID: "j", Scopes: []string{"tether.registry"}}
	readOnly := identity.Principal{Verified: true, ID: "msg://service/viewer/dev", Kind: "service", TokenID: "j", Scopes: []string{"tether.read"}}
	messagesOnly := identity.Principal{Verified: true, ID: "msg://service/mailer/dev", Kind: "service", TokenID: "j", Scopes: []string{"tether.messages"}}
	const urn = "/registry/agents/msg%3A%2F%2Fagent%2Fagent-mux%2Fagt_x"
	for _, tc := range []struct {
		who          string
		p            identity.Principal
		method, path string
		allow        bool
	}{
		{"registry-only", registryOnly, "GET", "/registry/agents?substrate=nanite&external_id=a%2Fb", true},
		{"registry-only", registryOnly, "GET", urn, true},
		{"registry-only", registryOnly, "POST", "/registry/agents", true},
		{"registry-only", registryOnly, "PATCH", urn, true},
		{"registry-only", registryOnly, "DELETE", urn, true},
		{"registry-only", registryOnly, "GET", "/messages/list", false},
		{"registry-only", registryOnly, "GET", "/messages/inbox", false},
		{"registry-only", registryOnly, "GET", "/messages/watch", false},
		{"registry-only", registryOnly, "GET", "/messages/abc", false},
		{"registry-only", registryOnly, "POST", "/messages", false},
		{"registry-only", registryOnly, "POST", "/messages/notify", false},
		{"registry-only", registryOnly, "GET", "/sessions", false},
		{"registry-only", registryOnly, "POST", "/sessions", false},
		{"registry-only", registryOnly, "GET", "/workers", false},
		{"read-only", readOnly, "GET", "/registry/agents", true},
		{"read-only", readOnly, "GET", urn, true},
		{"read-only", readOnly, "POST", "/registry/agents", false},
		{"read-only", readOnly, "DELETE", urn, false},
		{"messages-only", messagesOnly, "GET", "/registry/agents", false},
		{"messages-only", messagesOnly, "POST", "/registry/agents", false},
	} {
		got := ObserveLocalScope(httptest.NewRequest(tc.method, tc.path, nil), tc.p)
		if !got.Known || got.WouldAllow != tc.allow {
			t.Errorf("%s %s %s: would-allow=%v known=%v (required %q), want allow=%v", tc.who, tc.method, tc.path, got.WouldAllow, got.Known, got.Required, tc.allow)
		}
	}
}
