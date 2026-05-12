package skills

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sampleSkill = `---
id: refactor-go
name: Refactor Go
description: Apply Go refactoring patterns
triggers: [refactor, cleanup]
---

Use small focused commits. Prefer table-driven tests.
`

func TestParse_HappyPath(t *testing.T) {
	s, err := Parse([]byte(sampleSkill))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if s.ID != "refactor-go" {
		t.Errorf("ID = %q; want refactor-go", s.ID)
	}
	if s.Name != "Refactor Go" {
		t.Errorf("Name = %q", s.Name)
	}
	if got, want := strings.Join(s.Triggers, ","), "refactor,cleanup"; got != want {
		t.Errorf("Triggers = %q; want %q", got, want)
	}
	if !strings.Contains(s.Body, "Use small focused commits") {
		t.Errorf("Body missing expected content: %q", s.Body)
	}
}

func TestParse_RejectsMissingID(t *testing.T) {
	src := "---\nname: No ID\n---\nbody\n"
	if _, err := Parse([]byte(src)); err == nil {
		t.Fatal("expected error for missing id, got nil")
	}
}

func TestParse_RejectsMissingFrontmatter(t *testing.T) {
	src := "# Just a heading\nno frontmatter here\n"
	if _, err := Parse([]byte(src)); err == nil {
		t.Fatal("expected error for missing frontmatter, got nil")
	}
}

func TestParse_EOFTerminatedFrontmatterIsTolerated(t *testing.T) {
	// no closing --- means the whole file is frontmatter; legal but body empty
	src := "---\nid: only\nname: Only\n"
	s, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if s.ID != "only" {
		t.Errorf("ID = %q", s.ID)
	}
	if s.Body != "" {
		t.Errorf("Body should be empty, got %q", s.Body)
	}
}

func TestLoadDir_MissingIsEmpty(t *testing.T) {
	tmp := t.TempDir()
	out, err := LoadDir(filepath.Join(tmp, "does-not-exist"))
	if err != nil {
		t.Fatalf("LoadDir on missing dir should be silent: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("expected empty map, got %d entries", len(out))
	}
}

func TestCompileClaude_PerSkillFile(t *testing.T) {
	in := []Skill{
		{ID: "b", Name: "B", Body: "body B"},
		{ID: "a", Name: "A", Description: "alpha", Body: "body A"},
	}
	out := CompileClaude(in)
	if len(out) != 2 {
		t.Fatalf("want 2 compiled files, got %d", len(out))
	}
	// sorted: a, b
	if out[0].RelPath != filepath.Join(".claude", "skills", "a.md") {
		t.Errorf("first file RelPath = %q", out[0].RelPath)
	}
	if !strings.Contains(out[0].Content, "# A\n") || !strings.Contains(out[0].Content, "alpha") || !strings.Contains(out[0].Content, "body A") {
		t.Errorf("first file content missing fields: %q", out[0].Content)
	}
	if out[1].RelPath != filepath.Join(".claude", "skills", "b.md") {
		t.Errorf("second file RelPath = %q", out[1].RelPath)
	}
}

func TestCompileCodex_SingleAggregateFile(t *testing.T) {
	in := []Skill{
		{ID: "lint", Name: "Lint", Body: "lint body"},
		{ID: "format", Name: "Format", Description: "go fmt the world", Triggers: []string{"fmt"}, Body: "format body"},
	}
	out := CompileCodex(in)
	if len(out) != 1 {
		t.Fatalf("want 1 aggregated file, got %d", len(out))
	}
	if out[0].RelPath != "AGENTS.md" {
		t.Errorf("RelPath = %q; want AGENTS.md", out[0].RelPath)
	}
	body := out[0].Content
	if !strings.Contains(body, "## Skill: Format") || !strings.Contains(body, "## Skill: Lint") {
		t.Errorf("body missing skill sections: %q", body)
	}
	// "format" sorts before "lint" by ID — Format section should appear first.
	idxFormat := strings.Index(body, "## Skill: Format")
	idxLint := strings.Index(body, "## Skill: Lint")
	if idxFormat == -1 || idxLint == -1 || idxFormat > idxLint {
		t.Errorf("section order wrong: format=%d lint=%d", idxFormat, idxLint)
	}
	if !strings.Contains(body, "**Triggers:** fmt") {
		t.Errorf("body missing triggers line: %q", body)
	}
}

func TestCompileCodex_EmptyInputReturnsNil(t *testing.T) {
	if got := CompileCodex(nil); got != nil {
		t.Errorf("CompileCodex(nil) = %v; want nil", got)
	}
}

func TestCompileForProvider_AliasesRoute(t *testing.T) {
	in := []Skill{{ID: "x", Name: "X", Body: "b"}}
	cases := []struct {
		provider string
		wantExt  string
	}{
		{"claude", ".md"},
		{"claude-code", ".md"},
		{"CLAUDE-STREAM", ".md"},
		{"codex", "AGENTS.md"},
		{"codex-app-server", "AGENTS.md"},
	}
	for _, tc := range cases {
		out, err := CompileForProvider(tc.provider, in)
		if err != nil {
			t.Errorf("%s: %v", tc.provider, err)
			continue
		}
		if len(out) == 0 {
			t.Errorf("%s: no output", tc.provider)
			continue
		}
		if tc.wantExt == "AGENTS.md" {
			if out[0].RelPath != "AGENTS.md" {
				t.Errorf("%s: RelPath = %q", tc.provider, out[0].RelPath)
			}
		} else if !strings.HasSuffix(out[0].RelPath, tc.wantExt) {
			t.Errorf("%s: RelPath = %q", tc.provider, out[0].RelPath)
		}
	}
}

func TestCompileForProvider_UnknownReturnsSentinel(t *testing.T) {
	_, err := CompileForProvider("opencode", []Skill{{ID: "x", Body: "b"}})
	if !errors.Is(err, ErrUnsupportedProvider) {
		t.Errorf("err = %v; want ErrUnsupportedProvider", err)
	}
}

func TestWriteSkillFile_RoundTrip(t *testing.T) {
	orig := Skill{
		ID:          "round-trip",
		Name:        "Round Trip",
		Description: "ensure write→parse is lossless",
		Triggers:    []string{"a", "b"},
		Body:        "## Body heading\n\nSome content.\n",
	}
	var buf bytes.Buffer
	if err := WriteSkillFile(&buf, orig); err != nil {
		t.Fatalf("WriteSkillFile: %v", err)
	}
	got, err := Parse(buf.Bytes())
	if err != nil {
		t.Fatalf("Parse round-trip: %v", err)
	}
	if got.ID != orig.ID || got.Name != orig.Name || got.Description != orig.Description {
		t.Errorf("frontmatter lost: %+v", got)
	}
	if strings.Join(got.Triggers, ",") != strings.Join(orig.Triggers, ",") {
		t.Errorf("triggers lost: %v vs %v", got.Triggers, orig.Triggers)
	}
	if !strings.Contains(got.Body, "Body heading") {
		t.Errorf("body lost: %q", got.Body)
	}
}

func TestLoadDir_LoadsParsedFiles(t *testing.T) {
	tmp := t.TempDir()
	skillsDir := filepath.Join(tmp, "skills")
	mustWrite := func(name, body string) {
		t.Helper()
		path := filepath.Join(skillsDir, name)
		if err := writeFileEnsureDir(path, body); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("a.md", "---\nid: a\nname: A\n---\nbody A\n")
	mustWrite("b.md", "---\nid: b\nname: B\n---\nbody B\n")
	mustWrite("readme.txt", "ignored — not .md")
	out, err := LoadDir(skillsDir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 skills, got %d", len(out))
	}
	if out["a"].Name != "A" || out["b"].Name != "B" {
		t.Errorf("loaded skills wrong: %+v", out)
	}
}

func writeFileEnsureDir(path, body string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(body), 0o644)
}
