package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/config"
)

func TestValidateCatalogSkills(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		refs       []string
		want       string
	}{
		{name: "valid", body: "---\nid: example\n---\nBody", refs: []string{"example"}},
		{name: "unused missing id", body: "---\nname: Example\n---\nBody", want: "missing required field id"},
		{name: "referenced malformed YAML", body: "---\nid: [\n---\n", refs: []string{"example"}, want: "parse frontmatter"},
		{name: "missing reference", refs: []string{"absent"}, want: `agent "worker" references missing skill "absent"`},
		{name: "missing delimiter", body: "# Plain markdown", want: "missing leading ---"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			root := t.TempDir()
			if tc.body != "" {
				if err := os.MkdirAll(filepath.Join(root, "skills"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, "skills/example.md"), []byte(tc.body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			cat := &config.Catalog{Agents: map[string]config.Agent{"worker": {Skills: tc.refs}}}
			err := ValidateCatalog(root, cat)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "docs/skill-format.md") {
				t.Fatalf("error = %v, want %q and remedy", err, tc.want)
			}
		})
	}
}

func TestValidateCatalogChecksShadowedSkills(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := t.TempDir()
	for dir, body := range map[string]string{filepath.Join(root, "skills"): "---\nname: Missing ID\n---\n", filepath.Join(home, ".tether/skills"): "---\nid: example\n---\n"} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "example.md"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := ValidateCatalog(root, &config.Catalog{}); err == nil || !strings.Contains(err.Error(), "missing required field id") {
		t.Fatalf("error = %v", err)
	}
}
