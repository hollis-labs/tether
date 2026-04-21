package bootgen_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chrispian/agent-mux/internal/bootgen"
)

func TestLoadProfile_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	yaml := `id: test.engineer.main
display_name: "Test Engineer"
slots:
  agent:
    type: static
    path: agent.md
`
	if err := os.WriteFile(filepath.Join(dir, "test.engineer.main.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := bootgen.LoadProfile(filepath.Join(dir, "test.engineer.main.yaml"))
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	if p.ID != "test.engineer.main" {
		t.Errorf("ID = %q", p.ID)
	}
	if p.DisplayName != "Test Engineer" {
		t.Errorf("DisplayName = %q", p.DisplayName)
	}
}

func TestLoadProfiles_MissingDir(t *testing.T) {
	profiles, err := bootgen.LoadProfiles("/nonexistent/boot-profiles")
	if err != nil {
		t.Fatalf("expected no error for missing dir, got: %v", err)
	}
	if len(profiles) != 0 {
		t.Errorf("expected empty map, got %d profiles", len(profiles))
	}
}

func TestGenerate_StaticSlot(t *testing.T) {
	dir := t.TempDir()

	// Write an agent file.
	agentPath := filepath.Join(dir, "agent.md")
	if err := os.WriteFile(agentPath, []byte("# Test Agent\n\nI am a test."), 0o600); err != nil {
		t.Fatal(err)
	}

	p := bootgen.Profile{
		ID:          "test.engineer.main",
		DisplayName: "Test Engineer",
		Slots: map[string]bootgen.SlotSource{
			"agent": {Type: "static", Path: agentPath},
		},
	}

	var buf bytes.Buffer
	if err := bootgen.Generate(context.Background(), p, dir, &buf); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "Test Engineer") {
		t.Errorf("display name missing from output")
	}
	if !strings.Contains(out, "I am a test") {
		t.Errorf("agent slot content missing from output")
	}
}

func TestGenerate_CmdSlot(t *testing.T) {
	p := bootgen.Profile{
		ID: "test.cmd.main",
		Slots: map[string]bootgen.SlotSource{
			"history": {Type: "cmd", Run: "echo 'abc123 feat: test commit'"},
		},
	}

	var buf bytes.Buffer
	if err := bootgen.Generate(context.Background(), p, t.TempDir(), &buf); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	if !strings.Contains(buf.String(), "test commit") {
		t.Errorf("cmd slot output missing from rendered boot prompt")
	}
}

func TestGenerate_FailedSlotInline(t *testing.T) {
	p := bootgen.Profile{
		ID: "test.fail.main",
		Slots: map[string]bootgen.SlotSource{
			"memory": {Type: "cmd", Run: "exit 1"},
		},
	}

	var buf bytes.Buffer
	// Generate should succeed even when a slot fails.
	if err := bootgen.Generate(context.Background(), p, t.TempDir(), &buf); err != nil {
		t.Fatalf("Generate should not error on slot failure, got: %v", err)
	}
	if !strings.Contains(buf.String(), "slot:memory resolution failed") {
		t.Errorf("expected inline failure comment in output")
	}
}

func TestGenerate_DirectorySlot(t *testing.T) {
	dir := t.TempDir()
	skillsDir := filepath.Join(dir, "skills")
	if err := os.MkdirAll(skillsDir, 0o750); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"foo.md", "bar.md"} {
		if err := os.WriteFile(filepath.Join(skillsDir, f), []byte("# "+f), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	p := bootgen.Profile{
		ID: "test.dir.main",
		Slots: map[string]bootgen.SlotSource{
			"skills": {Type: "static", Path: skillsDir, Glob: "*.md"},
		},
	}

	var buf bytes.Buffer
	if err := bootgen.Generate(context.Background(), p, dir, &buf); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "foo.md") || !strings.Contains(out, "bar.md") {
		t.Errorf("directory slot files not in output: %s", out)
	}
}
