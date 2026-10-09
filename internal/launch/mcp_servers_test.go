package launch

import (
	"slices"
	"testing"
)

// CW-20261001-0228: a launched agent's proxy gets torque only unless
// a launch, project or boot profile names its own list.
func TestEffectiveMCPServers(t *testing.T) {
	def := []string{"torque"}
	for _, tc := range []struct {
		name string
		env  map[string]string
		want []string
	}{
		{"nil env", nil, def},
		{"unset", map[string]string{"OTHER": "x"}, def},
		{"empty grants none", map[string]string{MCPServersEnv: ""}, nil},
		{"only separators and blanks", map[string]string{MCPServersEnv: " , ,"}, nil},
		{"explicit list replaces the default", map[string]string{MCPServersEnv: "loom"}, []string{"loom"}},
		{"explicit list is not merged with the default", map[string]string{MCPServersEnv: "hadron,tesseract"}, []string{"hadron", "tesseract"}},
		{"whitespace trimmed, order kept", map[string]string{MCPServersEnv: " torque , cerberus,nanite "}, []string{"torque", "cerberus", "nanite"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := EffectiveMCPServers(tc.env); !slices.Equal(got, tc.want) {
				t.Fatalf("EffectiveMCPServers(%v) = %q; want %q", tc.env, got, tc.want)
			}
		})
	}
}

func TestDefaultMCPServers_IsExactlyTorque(t *testing.T) {
	if !slices.Equal(DefaultMCPServers, []string{"torque"}) {
		t.Fatalf("DefaultMCPServers = %q; the approved default is torque only (CW-20261001-0228)", DefaultMCPServers)
	}
	// cerberus controls infrastructure and is never on by default.
	if slices.Contains(DefaultMCPServers, "cerberus") {
		t.Fatal("cerberus is in the default allow-list")
	}
}

// The default is returned as a copy: a caller appending to it must not widen
// every later launch's default.
func TestEffectiveMCPServers_DefaultIsACopy(t *testing.T) {
	got := EffectiveMCPServers(nil)
	got[0] = "cerberus"
	_ = append(got, "nanite")
	if !slices.Equal(DefaultMCPServers, []string{"torque"}) {
		t.Fatalf("mutating a returned default changed DefaultMCPServers: %q", DefaultMCPServers)
	}
	if again := EffectiveMCPServers(nil); !slices.Equal(again, []string{"torque"}) {
		t.Fatalf("a later call returned %q", again)
	}
}
