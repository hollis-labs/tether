package config

import (
	"gopkg.in/yaml.v3"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestCatalogExpectedOrigins(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       []string
	}{
		{"omitted", "{}\n", nil},
		{"empty", "mcp:\n  expected_origins: []\n", []string{}},
		{"explicit", "mcp:\n  expected_origins: [registry, torque]\n", []string{"registry", "torque"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "global.yaml"), []byte(tc.body), 0600); err != nil {
				t.Fatal(err)
			}
			cat, err := LoadLayered(root)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(cat.Global.MCP.ExpectedOrigins, tc.want) {
				t.Fatalf("%#v want %#v", cat.Global.MCP.ExpectedOrigins, tc.want)
			}
			data, err := yaml.Marshal(cat.Global)
			if err != nil {
				t.Fatal(err)
			}
			var rewritten Global
			if err := yaml.Unmarshal(data, &rewritten); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(rewritten.MCP.ExpectedOrigins, tc.want) {
				t.Fatalf("catalog rewrite changed expectations: %#v want %#v", rewritten.MCP.ExpectedOrigins, tc.want)
			}
		})
	}
}
