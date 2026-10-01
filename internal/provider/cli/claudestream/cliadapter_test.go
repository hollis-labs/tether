package claudestream

import (
	"slices"
	"testing"
)

func TestBeforeEndOfOptions(t *testing.T) {
	extra := []string{"--add-dir", "/proj"}
	for _, tc := range []struct {
		name       string
		argv, want []string
	}{
		{"no marker appends", []string{"app-server"}, []string{"app-server", "--add-dir", "/proj"}},
		{"before the marker", []string{"exec", "--json", "--", "hi"}, []string{"exec", "--json", "--add-dir", "/proj", "--", "hi"}},
		{"prompt that is itself --", []string{"-p", "--", "--"}, []string{"-p", "--add-dir", "/proj", "--", "--"}},
	} {
		if got := beforeEndOfOptions(slices.Clone(tc.argv), extra); !slices.Equal(got, tc.want) {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
	if got := beforeEndOfOptions([]string{"-p", "--", "x"}, nil); !slices.Equal(got, []string{"-p", "--", "x"}) {
		t.Errorf("no extras changed argv: %q", got)
	}
}
