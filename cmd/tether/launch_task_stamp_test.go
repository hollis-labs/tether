package main

import "testing"

func TestLaunchTaskStamp(t *testing.T) {
	for _, tc := range []struct {
		in   []string
		want string
	}{
		{nil, ""},
		{[]string{"CW-20261016-0155"}, "CW-20261016-0155"},
		{[]string{" CW-20261016-0155 "}, "CW-20261016-0155"},
		{[]string{"CW-1", "CW-2"}, ""}, // several tasks: no single task to stamp
	} {
		if got := launchTaskStamp(tc.in); got != tc.want {
			t.Errorf("launchTaskStamp(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
