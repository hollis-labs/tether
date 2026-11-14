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
	s, err = Parse(map[string]any{"catalog_dir": "/data/tether/catalog", "retention_days": float64(90)})
	if err != nil || s.CatalogDir != "/data/tether/catalog" || s.RetentionDays != 90 {
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
		{map[string]any{"retention_days": "thirty"}, "tether.retention_days:"},
		{map[string]any{"retention_days": float64(1.5)}, "tether.retention_days:"},
		{map[string]any{"retention_days": float64(0)}, "tether.retention_days: must be between 1 and 3650"},
		{map[string]any{"retention_days": float64(3651)}, "tether.retention_days: must be between 1 and 3650"},
	} {
		if _, err := Parse(c.raw); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("Parse(%v) error = %v, want it to contain %q", c.raw, err, c.want)
		}
	}
}
