package launchprofile_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/tether/internal/launchprofile"
)

func TestFileSource(t *testing.T) {
	tmpDir := t.TempDir()

	// Write agent/profile YAML in agents/ subdirectory
	agentsDir := filepath.Join(tmpDir, "agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	agentYAML := `
id: test-agent
name: Test Agent
provider: claude-code
skills:
  - git
  - search
`
	if err := os.WriteFile(filepath.Join(agentsDir, "test-agent.yaml"), []byte(agentYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	// Write project context in projects/ subdirectory
	projectsDir := filepath.Join(tmpDir, "projects")
	if err := os.MkdirAll(projectsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	projectYAML := `
id: test-project
name: Test Project
repo_root: /dev/test
workspace:
  default_mode: worktree
`
	if err := os.WriteFile(filepath.Join(projectsDir, "test-project.yaml"), []byte(projectYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	fs := launchprofile.NewFileSource(tmpDir)
	ctx := context.Background()

	profile, err := fs.GetProfile(ctx, "test-agent")
	if err != nil {
		t.Fatalf("GetProfile failed: %v", err)
	}
	if profile.ID != "test-agent" || profile.Name != "Test Agent" || profile.Provider != "claude-code" {
		t.Errorf("Unexpected profile: %+v", profile)
	}
	if len(profile.Skills) != 2 || profile.Skills[0] != "git" {
		t.Errorf("Skills = %v", profile.Skills)
	}

	launchCtx, err := fs.GetContext(ctx, "test-project")
	if err != nil {
		t.Fatalf("GetContext failed: %v", err)
	}
	if launchCtx.ID != "test-project" || launchCtx.RepoRoot != "/dev/test" {
		t.Errorf("Unexpected context: %+v", launchCtx)
	}

	// Nonexistent profile
	_, err = fs.GetProfile(ctx, "nonexistent")
	if !errors.Is(err, launchprofile.ErrProfileNotFound) {
		t.Errorf("expected ErrProfileNotFound, got %v", err)
	}
}

func TestCairnBundleSource(t *testing.T) {
	tmpDir := t.TempDir()
	profilesDir := filepath.Join(tmpDir, "profiles")
	if err := os.MkdirAll(profilesDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Write markdown file with frontmatter
	content := `---
id: cairn-engineer
name: Cairn Engineer
provider: codex
skills:
  - analysis
---
# Instructions
You are an expert engineer.
`
	if err := os.WriteFile(filepath.Join(profilesDir, "cairn-engineer.md"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	cairn := launchprofile.NewCairnBundleSource(tmpDir)
	profile, err := cairn.GetProfile(context.Background(), "cairn-engineer")
	if err != nil {
		t.Fatalf("GetProfile failed: %v", err)
	}

	if profile.ID != "cairn-engineer" {
		t.Errorf("ID = %q, want cairn-engineer", profile.ID)
	}
	if profile.Provider != "codex" {
		t.Errorf("Provider = %q, want codex", profile.Provider)
	}
	if profile.Body != "# Instructions\nYou are an expert engineer." {
		t.Errorf("Body = %q", profile.Body)
	}
}

func TestMultiSource(t *testing.T) {
	mem1 := launchprofile.NewMemorySource()
	mem2 := launchprofile.NewMemorySource()

	mem1.AddProfile(&launchprofile.LaunchProfile{ID: "p1", Name: "Profile 1"})
	mem2.AddProfile(&launchprofile.LaunchProfile{ID: "p2", Name: "Profile 2"})

	multi := launchprofile.NewMultiSource(mem1, mem2)
	ctx := context.Background()

	p1, err := multi.GetProfile(ctx, "p1")
	if err != nil || p1.Name != "Profile 1" {
		t.Errorf("GetProfile(p1) = %v, %v", p1, err)
	}

	p2, err := multi.GetProfile(ctx, "p2")
	if err != nil || p2.Name != "Profile 2" {
		t.Errorf("GetProfile(p2) = %v, %v", p2, err)
	}

	_, err = multi.GetProfile(ctx, "p3")
	if !errors.Is(err, launchprofile.ErrProfileNotFound) {
		t.Errorf("expected ErrProfileNotFound, got %v", err)
	}
}
