package pulse

import "testing"

func TestMachineName(t *testing.T) {
	for _, tc := range []struct{ name, env, label, host, want string }{
		{"env overrides everything", "override", "home-laptop", "box.local", "override"},
		{"worker label beats the hostname", "", "home-laptop", "Chrispians-MacBook-Pro.local", "home-laptop"},
		{"no label uses the short hostname", "", "", "box.local", "box"},
		{"label equal to the hostname", "", "agent-os", "agent-os", "agent-os"},
		{"path separator is not a usable label", "", "../x", "box.local", "box"},
		{"slash is not a usable label", "", "a/b", "box.local", "box"},
		{"leading dot is not a usable label", "", ".hidden", "box.local", "box"},
		{"too long is not a usable label", "", "a123456789a123456789a123456789a123456789a123456789a123456789a1234", "box.local", "box"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := machineName(tc.env, tc.label, tc.host); got != tc.want {
				t.Fatalf("machineName(%q, %q, %q) = %q, want %q", tc.env, tc.label, tc.host, got, tc.want)
			}
		})
	}
}
