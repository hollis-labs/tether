package identity

import "testing"

// tether.registry_read is registry reads only. Every scope that already read the
// registry keeps doing so; a registry-only credential reads it without tether.read.
func TestRegistryReadContainment(t *testing.T) {
	for _, tc := range []struct {
		granted string
		need    string
		want    bool
	}{
		{"tether.registry_read", "tether.registry_read", true},
		{"tether.read", "tether.registry_read", true},
		{"tether.registry", "tether.registry_read", true},
		{"tether.maintain", "tether.registry_read", true},
		{"tether.admin", "tether.registry_read", true},
		{"tether.messages", "tether.registry_read", false},
		{"tether.events", "tether.registry_read", false},
		{"tether.operate", "tether.registry_read", false},
		// One direction only: reading the registry grants nothing else.
		{"tether.registry_read", "tether.read", false},
		{"tether.registry_read", "tether.registry", false},
		{"tether.registry", "tether.read", false},
		{"tether.registry", "tether.messages", false},
		// The bare names resolve to the same canonical scope.
		{"read", "registry_read", true},
	} {
		if got := ScopeGranted([]string{tc.granted}, tc.need); got != tc.want {
			t.Errorf("granted %q satisfies %q = %v, want %v", tc.granted, tc.need, got, tc.want)
		}
	}
	if !IsCanonicalScope("tether.registry_read") {
		t.Fatal("tether.registry_read must be a recognized canonical scope")
	}
}
