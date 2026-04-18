package workspace

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/chrispian/agent-mux/internal/launch"
)

func TestCreateLaysOutDirs(t *testing.T) {
	dir := t.TempDir()
	plan := &launch.Plan{LaunchID: "x", BootPrompt: "hello"}
	sess, err := Create(dir, "sess-1", plan)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	for _, sub := range Subdirs {
		if _, err := os.Stat(filepath.Join(sess.Root, sub)); err != nil {
			t.Fatalf("missing subdir %s: %v", sub, err)
		}
	}
	if b, err := os.ReadFile(sess.PromptPath); err != nil || string(b) != "hello" {
		t.Fatalf("prompt not written: err=%v b=%q", err, string(b))
	}
}
