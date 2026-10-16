package mcpadapter

import "testing"

func TestCheckScopeAcceptsDeviceScopesForChildScopes(t *testing.T) {
	a := New(nil, "tok", []string{"operate", "worker"})
	for _, scope := range []string{ScopeSessionWrite, ScopeMessageWrite} {
		if err := a.checkScope(scope); err != nil {
			t.Fatalf("%s: %v", scope, err)
		}
	}
	if err := a.checkScope(ScopeCatalogWrite); err == nil {
		t.Fatal("operate must not grant catalog.write")
	}
	// A bare "*" in a client-supplied scope list is not the daemon-verified wildcard.
	if err := New(nil, "tok", []string{"*"}).checkScope(ScopeSessionWrite); err == nil {
		t.Fatal("unverified * granted")
	}
}
