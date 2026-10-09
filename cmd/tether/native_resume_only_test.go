package main

import "testing"

func TestNativeOnlyBootSelectionBindsExplicitCoordinationWorkroot(t *testing.T) {
	oldIDs, oldRoots := bootNativeOnlySourceIDs, bootNativeOnlyWorkRoots
	t.Cleanup(func() { bootNativeOnlySourceIDs, bootNativeOnlyWorkRoots = oldIDs, oldRoots })
	const id = "9aa27f72-617b-484c-906d-0a7049f9a167"
	for _, tc := range []struct {
		name       string
		ids, roots []string
		valid      bool
	}{
		{"ordinary", nil, nil, true},
		{"selected", []string{id}, []string{id + "=/tmp/native-context"}, true},
		{"unselected_root", nil, []string{id + "=/tmp/native-context"}, false},
		{"relative_root", []string{id}, []string{id + "=relative"}, false},
		{"ambiguous_root", []string{id}, []string{id + "=/tmp/one", id + "=/tmp/two"}, false},
		{"invalid_source", []string{"not-canonical"}, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bootNativeOnlySourceIDs, bootNativeOnlyWorkRoots = tc.ids, tc.roots
			result, err := nativeOnlyBootOptions()
			if (err == nil) != tc.valid {
				t.Fatalf("acceptance %s: %v", tc.name, err)
			}
			if tc.name == "selected" && (len(result.NativeOnlySourceIDs) != 1 || result.NativeOnlyWorkRoots[id] != "/tmp/native-context") {
				t.Fatal("source-bound workroot lost")
			}
		})
	}
}
