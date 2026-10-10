package identity

import "testing"

func TestDeviceScopesNarrowOnly(t *testing.T) {
	for _, scopes := range [][]string{nil, {}, {"*"}, {"write"}, {"read", "unknown"}} {
		if _, err := NormalizeDeviceScopes(scopes); err == nil {
			t.Fatal("accepted invalid device scopes")
		}
	}
	got, err := NarrowDeviceScopes([]string{"read", "operate"}, []string{"read", "read"})
	if err != nil || len(got) != 1 || got[0] != "read" {
		t.Fatal(got, err)
	}
	if _, err := NarrowDeviceScopes([]string{"read"}, []string{"read", "admin"}); err == nil {
		t.Fatal("scope widening")
	}
	if HasDeviceScope(Principal{ID: "d", Kind: "device", Scopes: []string{"admin"}}, "operate") {
		t.Fatal("implicit hierarchy")
	}
	if HasDeviceScope(Principal{ID: "s", Kind: "service", Scopes: []string{"read"}}, "read") {
		t.Fatal("non-device scope authority")
	}
}
