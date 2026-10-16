//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A hook may cost the agent at most the write budget even when something holds the session's spool lock.
func TestHookClaudeGivesUpAtTheWriteBudget(t *testing.T) {
	spool := t.TempDir()
	if _, _, err := runHook(t, hookFixture(t, "SessionStart"), "claude", "--session", "held", "--spool", spool); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(spool, "held.jsonl"), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil { //nolint:gosec // G115: fd fits int
		t.Fatal(err)
	}
	start := time.Now()
	_, stderr, err := runHook(t, hookFixture(t, "PreToolUse-read"), "claude", "--session", "held", "--spool", spool)
	if err != nil || strings.TrimSpace(stderr) != "tether hook: timeout" {
		t.Fatalf("err=%v stderr=%q", err, stderr)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("a held lock cost the agent %s", took)
	}
}
