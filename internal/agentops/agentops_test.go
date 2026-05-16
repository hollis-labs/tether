package agentops

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/hollis-labs/tether/internal/config"
)

func TestParseScope(t *testing.T) {
	cases := []struct {
		in      string
		want    config.Layer
		wantErr bool
	}{
		{"", config.LayerProject, false},
		{"project", config.LayerProject, false},
		{"  PROJECT  ", config.LayerProject, false},
		{"user", config.LayerUser, false},
		{"system", config.LayerSystem, false},
		{"garbage", 0, true},
	}
	for _, tc := range cases {
		got, err := ParseScope(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseScope(%q): expected error", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseScope(%q): unexpected error %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseScope(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestValidID(t *testing.T) {
	for _, ok := range []string{"auditor", "stack-explorer-auditor", "a"} {
		if !ValidID(ok) {
			t.Errorf("ValidID(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", " ", ".", "..", "a/b", `a\b`, "../escape"} {
		if ValidID(bad) {
			t.Errorf("ValidID(%q) = true, want false", bad)
		}
	}
}

func TestCreate_WritesFileAndRejectsDuplicate(t *testing.T) {
	root := t.TempDir()
	path, err := Create(root, "auditor", Params{
		Name:         "Auditor",
		Roles:        []string{"auditor"},
		SystemPrompt: "you audit",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	want := filepath.Join(root, "agents", "auditor.yaml")
	if path != want {
		t.Errorf("path = %q, want %q", path, want)
	}

	var a config.Agent
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	if err := yaml.Unmarshal(data, &a); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if a.ID != "auditor" || a.Name != "Auditor" || a.SystemPrompt != "you audit" {
		t.Errorf("written agent = %+v", a)
	}
	if len(a.Roles) != 1 || a.Roles[0] != "auditor" {
		t.Errorf("roles = %v", a.Roles)
	}

	// A second create at the same path must fail with ErrExists (so callers
	// can classify it as a conflict) rather than clobber the file.
	_, err = Create(root, "auditor", Params{})
	if err == nil {
		t.Fatal("Create: expected duplicate error, got nil")
	}
	if !errors.Is(err, ErrExists) {
		t.Errorf("Create duplicate: error = %v, want errors.Is ErrExists", err)
	}
}

func TestCreate_RejectsBadID(t *testing.T) {
	if _, err := Create(t.TempDir(), "../escape", Params{}); err == nil {
		t.Fatal("Create: expected invalid-id error, got nil")
	}
}

func TestCreate_NameDefaultsToID(t *testing.T) {
	root := t.TempDir()
	if _, err := Create(root, "builder", Params{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	var a config.Agent
	data, _ := os.ReadFile(filepath.Join(root, "agents", "builder.yaml"))
	_ = yaml.Unmarshal(data, &a)
	if a.Name != "builder" {
		t.Errorf("Name = %q, want %q (defaulted from id)", a.Name, "builder")
	}
}

func TestUpdate_PatchesProvidedFieldsOnly(t *testing.T) {
	root := t.TempDir()
	path, err := Create(root, "auditor", Params{
		Name:         "Auditor",
		Roles:        []string{"auditor"},
		SystemPrompt: "original prompt",
		AgentPrompt:  "original persona",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Only name + roles change; empty SystemPrompt must leave it untouched,
	// and a non-nil Roles slice replaces the list.
	got, err := Update(path, Params{
		Name:  "Renamed Auditor",
		Roles: []string{"auditor", "reviewer"},
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got.Name != "Renamed Auditor" {
		t.Errorf("Name = %q, want renamed", got.Name)
	}
	if len(got.Roles) != 2 {
		t.Errorf("Roles = %v, want 2-element replacement", got.Roles)
	}
	if got.SystemPrompt != "original prompt" {
		t.Errorf("SystemPrompt = %q, want unchanged", got.SystemPrompt)
	}
	if got.AgentPrompt != "original persona" {
		t.Errorf("AgentPrompt = %q, want unchanged", got.AgentPrompt)
	}

	// Re-read from disk to confirm the patch persisted.
	var onDisk config.Agent
	data, _ := os.ReadFile(path)
	_ = yaml.Unmarshal(data, &onDisk)
	if onDisk.Name != "Renamed Auditor" || onDisk.SystemPrompt != "original prompt" {
		t.Errorf("on-disk agent = %+v", onDisk)
	}
}

func TestUpdate_ClearsListWithEmptySlice(t *testing.T) {
	root := t.TempDir()
	path, err := Create(root, "auditor", Params{
		Roles:  []string{"auditor"},
		Skills: []string{"audit"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// A non-nil empty slice replaces (clears) the list; a nil slice would
	// leave it unchanged.
	got, err := Update(path, Params{Roles: []string{}, Skills: []string{}})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(got.Roles) != 0 {
		t.Errorf("Roles = %v, want cleared", got.Roles)
	}
	if len(got.Skills) != 0 {
		t.Errorf("Skills = %v, want cleared", got.Skills)
	}
}

func TestUpdate_MissingFile(t *testing.T) {
	if _, err := Update(filepath.Join(t.TempDir(), "nope.yaml"), Params{Name: "x"}); err == nil {
		t.Fatal("Update: expected error for missing file, got nil")
	}
}
