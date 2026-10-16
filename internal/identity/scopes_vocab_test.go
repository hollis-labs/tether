package identity_test

import (
	"testing"

	"github.com/hollis-labs/tether/internal/identity"
)

// One vocabulary for device scopes and the worker/MCP child scopes
// (CW-20261014-0035): a paired worker device holding operate satisfies the
// session.write / message.write checks that used to need a separate grant.
func TestScopeGrantedUnifiesDeviceAndChildScopes(t *testing.T) {
	for _, c := range []struct {
		granted  []string
		required string
		want     bool
	}{
		{[]string{"operate", "worker"}, "session.write", true},
		{[]string{"tether.operate"}, "message.write", true},
		{[]string{"operate"}, "catalog.write", false},
		{[]string{"maintain"}, "catalog.write", true},
		{[]string{"admin"}, "registry.write", true},
		{[]string{"tether.maintain"}, "tether.registry", true},
		{[]string{"tether.registry"}, "registry.write", true},
		{[]string{"tether.registry"}, "tether.maintain", false}, // one direction only
		{[]string{"tether.registry"}, "catalog.write", false},
		{[]string{"tether.catalog"}, "catalog.write", true},
		{[]string{"operate"}, "registry.write", false},
		// narrow scopes: operate contains all five; none contains operate or each other
		{[]string{"operate"}, "tether.sessions", true},
		{[]string{"operate"}, "tether.ai", true},
		{[]string{"tether.sessions"}, "tether.operate", false},
		{[]string{"tether.messages"}, "message.write", true},
		{[]string{"tether.messages"}, "session.write", false},
		{[]string{"tether.ai"}, "team.write", false},
		{[]string{"admin"}, "session.write", false}, // ADR 0062: admin does not contain operate-level scopes
		{[]string{"maintain"}, "message.write", false},
		// session input: terminal or sessions, nothing broader by accident
		{[]string{"tether.sessions"}, "input", true},
		{[]string{"terminal"}, "input", true},
		{[]string{"operate"}, "input", false},
		{[]string{"tether.messages"}, "input", false},
		{[]string{"read"}, "session.write", false},
		{[]string{"session.write"}, "session.write", true},   // legacy credential keeps its meaning
		{[]string{"session.write"}, "tether.operate", false}, // ...and gains no canonical authority
		{[]string{"read"}, "tether.read", true},
		{[]string{"tether.read"}, "read", true},
		{[]string{"admin"}, "operate", false}, // independent capabilities, no hierarchy
		{[]string{"*"}, "session.write", true},
		{[]string{"tether.*"}, "tether.terminal", true},
		{[]string{"tether.*"}, "tesseract.read", false},
		{nil, "read", false},
	} {
		if got := identity.ScopeGranted(c.granted, c.required); got != c.want {
			t.Errorf("ScopeGranted(%v, %q) = %v, want %v", c.granted, c.required, got, c.want)
		}
	}
	got, err := identity.CanonicalScopes([]string{"message.write", "registry.write", "groups.write", "read"})
	if err != nil || len(got) != 4 || got[0] != "tether.groups" || got[1] != "tether.messages" || got[2] != "tether.read" || got[3] != "tether.registry" {
		t.Fatalf("CanonicalScopes: %v %v", got, err)
	}
	if _, err := identity.CanonicalScopes([]string{"bogus"}); err == nil {
		t.Fatal("unknown scope accepted")
	}
}
