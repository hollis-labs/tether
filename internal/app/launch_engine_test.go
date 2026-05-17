package app

import (
	"testing"

	"github.com/hollis-labs/tether/internal/config"
)

// catWithDefaults builds a minimal *config.Catalog whose launch-engine
// and specs-root defaults are set to the given values.
func catWithDefaults(engine, specsRoot string) *config.Catalog {
	cat := &config.Catalog{}
	cat.Global.Catalog.Defaults.LaunchEngine = engine
	cat.Global.Catalog.Defaults.LaunchSpecsRoot = specsRoot
	return cat
}

func TestResolveLaunchEngine(t *testing.T) {
	cases := []struct {
		name   string
		env    string // "" means env unset
		config string // catalog.defaults.launch_engine
		want   LaunchEngine
	}{
		{"default when both unset", "", "", EngineCatalog},
		{"config spec", "", "spec", EngineSpec},
		{"config catalog", "", "catalog", EngineCatalog},
		{"config unrecognized falls back to catalog", "", "bogus", EngineCatalog},
		{"env spec overrides empty config", "spec", "", EngineSpec},
		{"env spec overrides catalog config", "spec", "catalog", EngineSpec},
		{"env catalog overrides spec config (precedence)", "catalog", "spec", EngineCatalog},
		{"env unrecognized overrides spec config", "bogus", "spec", EngineCatalog},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.env == "" {
				t.Setenv(EnvLaunchEngine, "")
			} else {
				t.Setenv(EnvLaunchEngine, tc.env)
			}
			got := resolveLaunchEngine(catWithDefaults(tc.config, ""))
			if got != tc.want {
				t.Fatalf("resolveLaunchEngine(env=%q,config=%q) = %q, want %q",
					tc.env, tc.config, got, tc.want)
			}
		})
	}
}

// TestResolveLaunchEngineNilCatalog guards the no-catalog path: a nil
// catalog with no env var must still resolve to the default.
func TestResolveLaunchEngineNilCatalog(t *testing.T) {
	t.Setenv(EnvLaunchEngine, "")
	if got := resolveLaunchEngine(nil); got != EngineCatalog {
		t.Fatalf("resolveLaunchEngine(nil) = %q, want %q", got, EngineCatalog)
	}
	t.Setenv(EnvLaunchEngine, "spec")
	if got := resolveLaunchEngine(nil); got != EngineSpec {
		t.Fatalf("resolveLaunchEngine(nil, env=spec) = %q, want %q", got, EngineSpec)
	}
}

func TestResolveLaunchSpecsRoot(t *testing.T) {
	t.Run("empty when both unset", func(t *testing.T) {
		t.Setenv(EnvLaunchSpecsRoot, "")
		if got := resolveLaunchSpecsRoot(catWithDefaults("", "")); got != "" {
			t.Fatalf("resolveLaunchSpecsRoot = %q, want empty (let specresolve default)", got)
		}
	})
	t.Run("config value used", func(t *testing.T) {
		t.Setenv(EnvLaunchSpecsRoot, "")
		want := "/tmp/corpus-from-config"
		if got := resolveLaunchSpecsRoot(catWithDefaults("", want)); got != want {
			t.Fatalf("resolveLaunchSpecsRoot = %q, want %q", got, want)
		}
	})
	t.Run("env overrides config", func(t *testing.T) {
		t.Setenv(EnvLaunchSpecsRoot, "/tmp/corpus-from-env")
		got := resolveLaunchSpecsRoot(catWithDefaults("", "/tmp/corpus-from-config"))
		if got != "/tmp/corpus-from-env" {
			t.Fatalf("resolveLaunchSpecsRoot = %q, want env value", got)
		}
	})
}
