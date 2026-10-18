//go:build !windows

package agentevents

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A hook may cost the agent at most the write budget even when something holds the session's spool lock, and the dropped event
// is counted.
func TestRecordClaudeHookGivesUpAtTheWriteBudgetAndCountsTheDrop(t *testing.T) {
	spool := t.TempDir()
	if class := RecordClaudeHook(context.Background(), strings.NewReader(realHook(t, "SessionStart")), "held", spool); class != "" {
		t.Fatal(class)
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
	class := RecordClaudeHook(context.Background(), strings.NewReader(realHook(t, "PreToolUse-read")), "held", spool)
	if class != "timeout" {
		t.Fatalf("class %q, want timeout", class)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("a held lock cost the agent %s", took)
	}
	if drops := (&Spool{Dir: spool}).HookDrops("held"); drops["timeout"] != 1 {
		t.Fatalf("the dropped event was not counted: %v", drops)
	}
}
