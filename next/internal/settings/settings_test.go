package settings

import (
	"strings"
	"testing"
)

func TestParseDefaults(t *testing.T) {
	s, err := Parse(nil)
	if err != nil || s != Defaults() {
		t.Fatalf("Parse(nil) = %+v, %v", s, err)
	}
	s, err = Parse(map[string]any{"catalog_dir": "/data/tether/catalog"})
	if err != nil || s.CatalogDir != "/data/tether/catalog" {
		t.Fatalf("Parse = %+v, %v", s, err)
	}
}

func TestParseRefusesNamingTheKey(t *testing.T) {
	for _, c := range []struct {
		raw  any
		want string
	}{
		{"oops", "tether: must be a map"},
		{map[string]any{"catalgo_dir": "/x"}, "tether.catalgo_dir: unknown key"},
		{map[string]any{"catalog_dir": "relative/catalog"}, "tether.catalog_dir: must be an absolute path"},
		{map[string]any{"catalog_dir": 7}, "tether.catalog_dir:"},
		// Retention arrives in S8 with the old semantics; until then the key is unknown, never defaulted.
		{map[string]any{"retention_days": float64(90)}, "tether.retention_days: unknown key"},
	} {
		if _, err := Parse(c.raw); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("Parse(%v) error = %v, want it to contain %q", c.raw, err, c.want)
		}
	}
}
