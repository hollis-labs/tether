package main

import "testing"

func TestLaunchTaskStamp(t *testing.T) {
	for _, tc := range []struct {
		explicit string
		in       []string
		want     string
	}{
		{"", nil, ""},
		{"", []string{"CW-20261016-0155"}, "CW-20261016-0155"},
		{"", []string{" CW-20261016-0155 "}, "CW-20261016-0155"},
		{"", []string{"CW-1", "CW-2"}, ""}, // several tasks: no single task to stamp
		{"CW-9", nil, "CW-9"},              // --task alone
		{" CW-9 ", []string{"CW-1", "CW-2"}, "CW-9"},
		{"CW-9", []string{"CW-1"}, "CW-9"}, // an explicit --task wins
	} {
		if got := launchTaskStamp(tc.explicit, tc.in); got != tc.want {
			t.Errorf("launchTaskStamp(%q, %v) = %q, want %q", tc.explicit, tc.in, got, tc.want)
		}
	}
}
