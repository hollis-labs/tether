package config

import (
	"gopkg.in/yaml.v3"
	"testing"
)

func TestIdentityConfigDefaultsAndValidation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, mode := range []string{"", "off", "observe", "enforce", "Observe", "typo"} {
		t.Run(mode, func(t *testing.T) {
			var global Global
			body := "identity:\n  mode: " + mode + "\n"
			if err := yaml.Unmarshal([]byte(body), &global); err != nil {
				t.Fatal(err)
			}
			cat := &Catalog{Global: global}
			err := cat.Validate()
			if err != nil {
				t.Fatalf("validate mode %q: %v", mode, err)
			}
			if mode == "" && global.Identity.EffectiveMode() != "observe" {
				t.Fatal("omitted mode must observe")
			}
		})
	}
}

func TestEnvModeCanOnlyTighten(t *testing.T) {
	cases := []struct{ cfg, env, want string }{
		{"enforce", "off", "enforce"},
		{"enforce", "observe", "enforce"},
		{"observe", "off", "observe"},
		{"observe", "enforce", "enforce"},
		{"off", "enforce", "enforce"},
		{"", "off", "observe"},
		{"enforce", "", "enforce"},
	}
	for _, c := range cases {
		t.Setenv("HOLLIS_AUTH_TETHER", c.env)
		t.Setenv("HOLLIS_AUTH", "")
		if got := (IdentityConfig{Mode: c.cfg}).EffectiveMode(); got != c.want {
			t.Errorf("cfg=%q env=%q: got %q want %q", c.cfg, c.env, got, c.want)
		}
	}
}
