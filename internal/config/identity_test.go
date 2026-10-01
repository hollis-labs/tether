package config

import (
	"gopkg.in/yaml.v3"
	"testing"
)

func TestIdentityConfigDefaultsAndValidation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, mode := range []string{"", "off", "observe", "enforce", "typo"} {
		t.Run(mode, func(t *testing.T) {
			var global Global
			body := "identity:\n  mode: " + mode + "\n"
			if err := yaml.Unmarshal([]byte(body), &global); err != nil {
				t.Fatal(err)
			}
			cat := &Catalog{Global: global}
			err := cat.Validate()
			if (err != nil) != (mode == "typo") {
				t.Fatalf("validate mode %q: %v", mode, err)
			}
			if mode == "" && global.Identity.EffectiveMode() != "observe" {
				t.Fatal("omitted mode must observe")
			}
		})
	}
}
