package bootgen_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/bootgen"
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

func TestLoadProfile_TesseractPrimary(t *testing.T) {
	dir := t.TempDir()
	yaml := `id: nanite.backend.main
display_name: "Nanite Backend"
identity:
  lineage_alias: nanite.backend.main
  tesseract_primary: "2026-04-19"
slots:
  agent:
    type: static
    path: agent.md
`
	if err := os.WriteFile(filepath.Join(dir, "agent.md"), []byte("# Agent"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "nanite.backend.main.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := bootgen.LoadProfile(path)
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	if p.Identity.TesseractPrimary != "2026-04-19" {
		t.Fatalf("TesseractPrimary = %q, want 2026-04-19", p.Identity.TesseractPrimary)
	}
	var buf bytes.Buffer
	if err := bootgen.Generate(context.Background(), p, dir, &buf); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !strings.Contains(buf.String(), `tesseract_primary: 2026-04-19`) {
		t.Fatalf("generated prompt missing tesseract primary:\n%s", buf.String())
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

func TestGenerate_RoleSummarySlot(t *testing.T) {
	dir := t.TempDir()
	rolePath := filepath.Join(dir, "backend.md")
	if err := os.WriteFile(rolePath, []byte(`# Backend Engineer

## Mission

Build compact, reliable backend changes that respect existing interfaces and leave enough context for the next operator.

## Detailed Operating Rules

- This line should stay in the full role file.
- This one should not be inlined either.
`), 0o600); err != nil {
		t.Fatal(err)
	}

	p := bootgen.Profile{
		ID: "test.role-summary.main",
		Slots: map[string]bootgen.SlotSource{
			"agent": {Type: "role_summary", Path: rolePath},
		},
	}

	var buf bytes.Buffer
	if err := bootgen.Generate(context.Background(), p, dir, &buf); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	out := buf.String()
	for _, want := range []string{
		"### Role Identity",
		"- Role: Backend Engineer",
		"- Full role file: `" + rolePath + "`",
		"Build compact, reliable backend changes",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("role summary missing %q in:\n%s", want, out)
		}
	}
	for _, leaked := range []string{"Detailed Operating Rules", "This line should stay in the full role file"} {
		if strings.Contains(out, leaked) {
			t.Errorf("role summary leaked full role content %q in:\n%s", leaked, out)
		}
	}
}

func TestGenerate_RoleSummarySlotTrimsRoleHeadingPrefix(t *testing.T) {
	dir := t.TempDir()
	rolePath := filepath.Join(dir, "worker.md")
	if err := os.WriteFile(rolePath, []byte(`# Role: Backend

## Identity

You are a backend engineer focused on APIs, services, data pipelines, and system reliability.
`), 0o600); err != nil {
		t.Fatal(err)
	}

	p := bootgen.Profile{
		ID: "test.role-summary-role-prefix.main",
		Slots: map[string]bootgen.SlotSource{
			"agent": {Type: "role_summary", Path: rolePath},
		},
	}

	var buf bytes.Buffer
	if err := bootgen.Generate(context.Background(), p, dir, &buf); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "- Role: Backend") {
		t.Fatalf("role summary did not normalize role heading:\n%s", out)
	}
	if strings.Contains(out, "- Role: Role: Backend") {
		t.Fatalf("role summary duplicated role prefix:\n%s", out)
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

func TestGenerate_SkillIndexSlot(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	writeBootgenSkillFixture(t, dir, "plan", `---
id: plan
name: Plan
description: Plan work before editing.
---

This body should not be in the boot prompt.
`)
	writeBootgenSkillFixture(t, dir, "refactor-go", `---
id: refactor-go
name: Refactor Go
description: Apply Go refactoring patterns.
---

Nor should this body.
`)

	p := bootgen.Profile{
		ID: "test.skill-index.main",
		Slots: map[string]bootgen.SlotSource{
			"skills": {Type: "skill_index"},
		},
	}

	var buf bytes.Buffer
	if err := bootgen.Generate(context.Background(), p, dir, &buf); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"/plan — Plan work before editing.",
		"/refactor-go — Apply Go refactoring patterns.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("skill index missing %q in:\n%s", want, out)
		}
	}
	for _, body := range []string{"This body should not be in the boot prompt.", "Nor should this body."} {
		if strings.Contains(out, body) {
			t.Errorf("skill index leaked body %q in:\n%s", body, out)
		}
	}
}

func TestGenerate_SkillIndexSlotRanksByProfileRelevance(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	writeBootgenSkillFixture(t, dir, "capture", `---
id: capture
description: Capture session outcomes.
triggers: [nanite, backend]
---
`)
	writeBootgenSkillFixture(t, dir, "refactor-go", `---
id: refactor-go
description: Apply Go refactoring patterns.
triggers: [refactor, backend]
---
`)
	writeBootgenSkillFixture(t, dir, "adr", `---
id: adr
description: Capture architecture decisions.
triggers: [decision]
---
`)

	p := bootgen.Profile{
		ID:   "nanite.backend.main",
		Tags: []string{"refactor"},
		Identity: bootgen.Identity{
			Role:    "backend",
			Project: "nanite",
		},
		Slots: map[string]bootgen.SlotSource{
			"skills": {Type: "skill_index", Limit: 2},
		},
	}

	var buf bytes.Buffer
	if err := bootgen.Generate(context.Background(), p, dir, &buf); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	lines := skillIndexLines(t, buf.String())
	if got, want := lines[0], "/capture — Capture session outcomes."; got != want {
		t.Fatalf("first ranked skill = %q, want %q\nfull output:\n%s", got, want, buf.String())
	}
	if got, want := lines[1], "/refactor-go — Apply Go refactoring patterns."; got != want {
		t.Fatalf("second ranked skill = %q, want %q\nfull output:\n%s", got, want, buf.String())
	}
}

func TestGenerate_SkillIndexSlotPreferTriggersNudgesTie(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	writeBootgenSkillFixture(t, dir, "capture", `---
id: capture
description: Capture session outcomes.
triggers: [backend, capture]
---
`)
	writeBootgenSkillFixture(t, dir, "refactor-go", `---
id: refactor-go
description: Apply Go refactoring patterns.
triggers: [backend, refactor]
---
`)

	p := bootgen.Profile{
		ID: "nanite.backend.main",
		Identity: bootgen.Identity{
			Role: "backend",
		},
		Slots: map[string]bootgen.SlotSource{
			"skills": {Type: "skill_index", Limit: 2, PreferTriggers: []string{"refactor"}},
		},
	}

	var buf bytes.Buffer
	if err := bootgen.Generate(context.Background(), p, dir, &buf); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	lines := skillIndexLines(t, buf.String())
	if got, want := lines[0], "/refactor-go — Apply Go refactoring patterns."; got != want {
		t.Fatalf("preferred trigger did not nudge ordering: got %q want %q\nfull output:\n%s", got, want, buf.String())
	}
}

func TestGenerate_SkillIndexSlotPriorityBreaksTriggerTie(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	writeBootgenSkillFixture(t, dir, "capture", `---
id: capture
description: Capture session outcomes.
triggers: [backend]
priority: 10
---
`)
	writeBootgenSkillFixture(t, dir, "refactor-go", `---
id: refactor-go
description: Apply Go refactoring patterns.
triggers: [backend]
priority: 5
---
`)

	p := bootgen.Profile{
		ID: "nanite.backend.main",
		Identity: bootgen.Identity{
			Role: "backend",
		},
		Slots: map[string]bootgen.SlotSource{
			"skills": {Type: "skill_index", Limit: 2},
		},
	}

	var buf bytes.Buffer
	if err := bootgen.Generate(context.Background(), p, dir, &buf); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	lines := skillIndexLines(t, buf.String())
	if got, want := lines[0], "/capture — Capture session outcomes."; got != want {
		t.Fatalf("priority did not break trigger tie: got %q want %q\nfull output:\n%s", got, want, buf.String())
	}
}

func skillIndexLines(t *testing.T, out string) []string {
	t.Helper()
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "/") {
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		t.Fatalf("no skill index lines found in output:\n%s", out)
	}
	return lines
}

func TestGenerate_HTTPSlot_BodyCap(t *testing.T) {
	// Server returns 8 MiB of bytes; bootgen caps http slot bodies at 4 MiB.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		body := bytes.Repeat([]byte("a"), 8*1024*1024)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	p := bootgen.Profile{
		ID: "test.http.cap",
		Slots: map[string]bootgen.SlotSource{
			"memory": {Type: "http", URL: srv.URL, Timeout: "5s"},
		},
	}

	var buf bytes.Buffer
	if err := bootgen.Generate(context.Background(), p, t.TempDir(), &buf); err != nil {
		t.Fatalf("Generate should not error on slot failure, got: %v", err)
	}
	// The slot resolution should fail inline (bootgen swallows slot errors and
	// records a placeholder); the body cap message should be threaded through.
	out := buf.String()
	if !strings.Contains(out, "slot:memory resolution failed") {
		t.Errorf("expected inline failure marker for oversize http slot; got: %s", out)
	}
	if !strings.Contains(out, "slot cap") {
		t.Errorf("expected slot-cap mention in failure inline; got: %s", out)
	}
}

func TestGenerate_HTTPSlot_RelativeRecallURLUsesBootBase(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.URL.String(), "/v1/recall?format=brief"; got != want {
			t.Fatalf("request URL = %q, want %q", got, want)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"memory_key":"recap","summary":"Recovered launch context."}],"meta":{"namespace":"test","returned":1}}`))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("TETHER_BOOT_HTTP_BASE_URL", srv.URL)
	t.Setenv("TESSERACT_URL", "")

	p := bootgen.Profile{
		ID: "test.http.relative",
		Slots: map[string]bootgen.SlotSource{
			"memory": {
				Type:           "http",
				URL:            "${TESSERACT_URL}/v1/recall?format=brief",
				ResponseFormat: "tesseract_recall",
			},
		},
	}

	var buf bytes.Buffer
	if err := bootgen.Generate(context.Background(), p, t.TempDir(), &buf); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "slot:memory resolution failed") {
		t.Fatalf("relative URL was not resolved through boot base:\n%s", out)
	}
	if !strings.Contains(out, "Recovered launch context.") {
		t.Fatalf("formatted recall body missing from output:\n%s", out)
	}
}

func writeBootgenSkillFixture(t *testing.T, root, id, body string) {
	t.Helper()
	path := filepath.Join(root, "skills", id+".md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
