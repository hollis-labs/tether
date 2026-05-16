package skills

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestDiscoverLayered_IncludesLegacySkillDirs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	systemRoot := t.TempDir()
	workingDir := t.TempDir()

	if err := writeFileEnsureDir(filepath.Join(systemRoot, "skills", "catalog.md"), "---\nid: catalog\nname: Catalog\n---\nCatalog body.\n"); err != nil {
		t.Fatal(err)
	}
	if err := writeFileEnsureDir(filepath.Join(home, ".tether", "skills", "legacy.md"), "---\nid: legacy\nname: Legacy Skill\ndescription: Use the old skill shape.\n---\nLegacy body.\n"); err != nil {
		t.Fatal(err)
	}
	if err := writeFileEnsureDir(filepath.Join(home, ".nanite", "skills", "nanite.md"), "# Nanite Skill\n\nLegacy Nanite skill.\n"); err != nil {
		t.Fatal(err)
	}

	got, err := DiscoverLayered(systemRoot, workingDir)
	if err != nil {
		t.Fatalf("DiscoverLayered: %v", err)
	}
	byID := map[string]LayeredSkill{}
	for _, s := range got {
		byID[s.Skill.ID] = s
	}
	for _, id := range []string{"catalog", "legacy", "nanite"} {
		if _, ok := byID[id]; !ok {
			t.Fatalf("missing %s in discovered skills: %+v", id, got)
		}
	}
	if byID["catalog"].Layer != "system" {
		t.Errorf("catalog layer = %q", byID["catalog"].Layer)
	}
	if byID["legacy"].Layer != "user" {
		t.Errorf("legacy layer = %q", byID["legacy"].Layer)
	}
	if byID["nanite"].Layer != "legacy-nanite" {
		t.Errorf("nanite layer = %q", byID["nanite"].Layer)
	}
	if byID["legacy"].Skill.Name != "Legacy Skill" {
		t.Errorf("legacy name = %q", byID["legacy"].Skill.Name)
	}
	if !strings.Contains(byID["legacy"].Skill.Description, "old skill shape") {
		t.Errorf("legacy description = %q", byID["legacy"].Skill.Description)
	}
}

func TestResolveLayered_ParsesOnlyRequestedSkill(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	systemRoot := t.TempDir()
	workingDir := t.TempDir()

	if err := writeFileEnsureDir(filepath.Join(systemRoot, "skills", "target.md"), "---\nid: target\nname: Target\n---\nTarget body.\n"); err != nil {
		t.Fatal(err)
	}
	if err := writeFileEnsureDir(filepath.Join(systemRoot, "skills", "broken.md"), "missing frontmatter\n"); err != nil {
		t.Fatal(err)
	}
	if err := writeFileEnsureDir(filepath.Join(home, ".tether", "skills", "also-broken.md"), "---\nname: Missing ID\n---\n"); err != nil {
		t.Fatal(err)
	}

	got, err := ResolveLayered(systemRoot, workingDir, "target")
	if err != nil {
		t.Fatalf("ResolveLayered: %v", err)
	}
	if got.Skill.ID != "target" {
		t.Errorf("ID = %q; want target", got.Skill.ID)
	}
}
