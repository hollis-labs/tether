package workerruntime

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestWithResumeReplacesResumeLikeFlags(t *testing.T) {
	got := withResume([]string{"--model", "m", "--resume", "stale", "-c", "--session-id", "x", "--fork-session", "--add-dir", "d"}, "abc-123")
	want := []string{"--model", "m", "--add-dir", "d", "--resume", "abc-123"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args = %v, want %v", got, want)
	}
}

// transcript writes a claude transcript for id whose first records carry cwd.
func transcript(t *testing.T, cfg, id, cwd string) {
	t.Helper()
	dir := filepath.Join(cfg, "projects", "-some-project")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"type":"summary"}` + "\n" + `{"type":"user","cwd":"` + cwd + `","sessionId":"` + id + `"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestResolveResumeCwd(t *testing.T) {
	root, cfg := t.TempDir(), t.TempDir()
	e := &Executor{Root: root, ClaudeDir: cfg}
	work := filepath.Join(root, "workspaces", "old-sess", "workroot")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	transcript(t, cfg, "good-1", work)
	transcript(t, cfg, "outside-1", t.TempDir())
	transcript(t, cfg, "gone-1", filepath.Join(root, "workspaces", "reaped", "workroot"))
	transcript(t, cfg, "rootws-1", filepath.Join(root, "workspaces"))

	got, err := e.resolveResumeCwd(nil, "good-1")
	if err != nil || got != work {
		t.Fatalf("good: cwd = %q, err = %v, want %q", got, err, work)
	}
	for name, id := range map[string]string{
		"missing session": "nope-1",
		"outside tree":    "outside-1",
		"workspace gone":  "gone-1",
		"workspaces root": "rootws-1",
		"path traversal":  "../x",
		"option-like":     "--evil",
		"empty":           "",
		"slash":           "a/b",
	} {
		_, err := e.resolveResumeCwd(nil, id)
		if !errors.Is(err, ErrResumeUnavailable) {
			t.Errorf("%s (%q): err = %v, want ErrResumeUnavailable", name, id, err)
		}
	}
}

func TestResolveResumeCwdTranscriptWithoutCwd(t *testing.T) {
	root, cfg := t.TempDir(), t.TempDir()
	dir := filepath.Join(cfg, "projects", "p")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "nocwd-1.jsonl"), []byte(`{"type":"summary"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := (&Executor{Root: root, ClaudeDir: cfg}).resolveResumeCwd(nil, "nocwd-1")
	if !errors.Is(err, ErrResumeUnavailable) || !strings.Contains(err.Error(), "no recorded workspace") {
		t.Fatalf("err = %v", err)
	}
}
