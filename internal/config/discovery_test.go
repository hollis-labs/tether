package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDiscovery_LayerPrecedence(t *testing.T) {
	tmp := t.TempDir()

	systemRoot := filepath.Join(tmp, "system")
	userRoot := filepath.Join(tmp, "user", ".agent-mux")
	projRoot := filepath.Join(tmp, "proj", ".agent-mux")

	// shared id present in all three layers
	writeAgent(t, systemRoot, "shared", "Shared System")
	writeAgent(t, userRoot, "shared", "Shared User")
	writeAgent(t, projRoot, "shared", "Shared Project")

	// system-only agent
	writeAgent(t, systemRoot, "system-only", "System Only")
	// user-only agent
	writeAgent(t, userRoot, "user-only", "User Only")
	// project-only agent
	writeAgent(t, projRoot, "project-only", "Project Only")

	// user overrides system for this id; project does not touch it
	writeAgent(t, systemRoot, "user-overrides", "User-Overrides System Version")
	writeAgent(t, userRoot, "user-overrides", "User-Overrides User Version")

	// skill paths come from each layer
	writeFile(t, filepath.Join(systemRoot, "skills", "refactor.md"), "# system skill")
	writeFile(t, filepath.Join(projRoot, "skills", "refactor.md"), "# project skill")
	writeFile(t, filepath.Join(userRoot, "skills", "lint.md"), "# user skill")

	layers := []LayerSpec{
		{Layer: LayerSystem, Root: systemRoot},
		{Layer: LayerUser, Root: userRoot},
		{Layer: LayerProject, Root: projRoot},
	}

	cat, err := Discover(layers)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}

	// project wins for the triple-shared id
	if got := cat.Agents["shared"].Agent.Name; got != "Shared Project" {
		t.Errorf("shared agent name = %q; want Shared Project (project layer should win)", got)
	}
	if got := cat.Agents["shared"].Layer; got != LayerProject {
		t.Errorf("shared agent layer = %s; want project", got)
	}

	// user overrides system when project layer is silent
	if got := cat.Agents["user-overrides"].Agent.Name; got != "User-Overrides User Version" {
		t.Errorf("user-overrides agent name = %q; want User-Overrides User Version", got)
	}
	if got := cat.Agents["user-overrides"].Layer; got != LayerUser {
		t.Errorf("user-overrides layer = %s; want user", got)
	}

	// origin layers preserved for non-collision IDs
	if got := cat.Agents["system-only"].Layer; got != LayerSystem {
		t.Errorf("system-only layer = %s; want system", got)
	}
	if got := cat.Agents["user-only"].Layer; got != LayerUser {
		t.Errorf("user-only layer = %s; want user", got)
	}
	if got := cat.Agents["project-only"].Layer; got != LayerProject {
		t.Errorf("project-only layer = %s; want project", got)
	}

	// skill path wins by layer
	if got := cat.SkillPaths["refactor"].Layer; got != LayerProject {
		t.Errorf("refactor skill layer = %s; want project", got)
	}
	if got := cat.SkillPaths["lint"].Layer; got != LayerUser {
		t.Errorf("lint skill layer = %s; want user", got)
	}
}

func TestDiscovery_MissingLayersAreSilent(t *testing.T) {
	tmp := t.TempDir()

	// Only the user layer exists on disk; system + project roots are nonexistent.
	userRoot := filepath.Join(tmp, "user", ".agent-mux")
	writeAgent(t, userRoot, "only-agent", "Only Agent")

	layers := []LayerSpec{
		{Layer: LayerSystem, Root: filepath.Join(tmp, "nope-system")},
		{Layer: LayerUser, Root: userRoot},
		{Layer: LayerProject, Root: filepath.Join(tmp, "nope-project")},
	}

	cat, err := Discover(layers)
	if err != nil {
		t.Fatalf("Discover with missing layers: %v", err)
	}
	if got := len(cat.Agents); got != 1 {
		t.Errorf("expected 1 agent, got %d", got)
	}
}

func TestDefaultLayers_StackShape(t *testing.T) {
	layers := DefaultLayers("/tmp/system", "/tmp/cwd")
	if len(layers) < 2 {
		t.Fatalf("expected at least system + user layers, got %d", len(layers))
	}
	if layers[0].Layer != LayerSystem {
		t.Errorf("first layer = %s; want system", layers[0].Layer)
	}
	last := layers[len(layers)-1]
	if last.Layer != LayerProject {
		t.Errorf("last layer = %s; want project", last.Layer)
	}
	if want := filepath.Join("/tmp/cwd", ".tether"); last.Root != want {
		t.Errorf("project root = %q; want %q", last.Root, want)
	}
}

// ─── helpers ─────────────────────────────────────────────────────────────────

func writeAgent(t *testing.T, root, id, name string) {
	t.Helper()
	dir := filepath.Join(root, "agents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	body := "id: " + id + "\nname: " + name + "\npermissions:\n  network: false\n"
	writeFile(t, filepath.Join(dir, id+".yaml"), body)
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
