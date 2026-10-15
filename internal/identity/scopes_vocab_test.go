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
	if err != nil || len(got) != 3 || got[0] != "tether.maintain" || got[1] != "tether.operate" || got[2] != "tether.read" {
		t.Fatalf("CanonicalScopes: %v %v", got, err)
	}
	if _, err := identity.CanonicalScopes([]string{"bogus"}); err == nil {
		t.Fatal("unknown scope accepted")
	}
}
