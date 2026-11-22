package app

import (
	"path/filepath"
	"testing"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/skills"
)

// Validation must accept references resolved by the same roots and filename
// keys as launch, including user-only skills and higher-precedence overrides.
func TestCatalogSkillValidationLaunchParity(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		system, user, project bool
		want                  string
	}{
		{name: "system", system: true, want: "system"},
		{name: "user only", user: true, want: "user"},
		{name: "user overrides system", system: true, user: true, want: "user"},
		{name: "project overrides user", system: true, user: true, project: true, want: "project"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			root := t.TempDir()
			for _, layer := range []struct {
				enabled  bool
				root, id string
			}{
				{tc.system, root, "system"},
				{tc.user, filepath.Join(home, ".tether"), "user"},
				{tc.project, filepath.Join(root, ".tether"), "project"},
			} {
				if layer.enabled {
					// The filename is the lookup key; the parser's id is deliberately
					// different, to catch a validator that silently changes lookup rules.
					writeCat(t, layer.root, "skills/shared.md", "---\nid: "+layer.id+"\n---\n")
				}
			}
			cat := &config.Catalog{Agents: map[string]config.Agent{"worker": {Skills: []string{"shared"}}}}
			if err := skills.ValidateCatalog(root, cat); err != nil {
				t.Fatal(err)
			}
			svc := &Service{CatalogRoot: root}
			loaded, err := svc.loadEffectiveSkills(cat.Agents["worker"].Skills)
			if err != nil {
				t.Fatal(err)
			}
			if len(loaded) != 1 || loaded[0].ID != tc.want {
				t.Fatalf("launch skills = %+v, want layer %q", loaded, tc.want)
			}
		})
	}
}
