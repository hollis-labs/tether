package proxyevents

import "testing"

func TestTruncateUTF8(t *testing.T) {
	for _, tc := range []struct {
		in   string
		n    int
		want string
	}{
		{"abc", 10, "abc"},
		{"abc", 3, "abc"},
		{"abcd", 3, "abc"},
		{"a€b", 2, "a"}, // € is bytes 1..3; a cut at 2 backs off to 1
		{"a€b", 4, "a€"},
		{"€", 1, ""},
	} {
		if got := truncateUTF8(tc.in, tc.n); got != tc.want {
			t.Errorf("truncateUTF8(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
	}
}
