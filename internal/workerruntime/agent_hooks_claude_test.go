package workerruntime

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/agentevents"
	"github.com/hollis-labs/tether/internal/workerwire"
)

// Opt-in (a real model call): TETHER_TEST_REAL_CLAUDE=1. The planted worker hooks fire in a real `claude -p`, record
// content-free events to the session spool, and the project's own hook still fires next to them (--settings adds a layer, it
// does not replace the hooks of the others).
func TestPlantedHooksFireInRealClaudeAlongsideProjectHooks(t *testing.T) {
	if os.Getenv("TETHER_TEST_REAL_CLAUDE") != "1" {
		t.Skip("set TETHER_TEST_REAL_CLAUDE=1 to run a real claude -p against the planted hooks")
	}
	claude, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude is not installed")
	}
	root := t.TempDir()
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(root, "tether-worker")
	build := exec.Command("go", "build", "-o", binary, "github.com/hollis-labs/tether/cmd/tether-worker")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build tether-worker: %v\n%s", err, out)
	}
	if err := os.Chmod(binary, 0o755); err != nil { // go build honors the umask; the hook binary check wants no group/other write
		t.Fatal(err)
	}
	ws := filepath.Join(root, "ws")
	work := filepath.Join(ws, "workroot")
	if err := os.MkdirAll(filepath.Join(work, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	projectHook := filepath.Join(root, "project-hook.sh")
	fired := filepath.Join(root, "project-fired.txt")
	if err := os.WriteFile(projectHook, []byte("#!/bin/sh\ncat >/dev/null\necho fired >> '"+fired+"'\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, ".claude", "settings.json"), []byte(`{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"`+projectHook+`"}]}]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "hello.txt"), []byte("hello from the real test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := &Executor{Binary: binary}
	extra := e.agentHookArgs(ws, workerwire.LaunchSpec{Session: "real-1", AgentEvents: true})
	if len(extra) != 2 {
		t.Fatal("hooks were not planted")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	args := append([]string{"-p", "Read hello.txt and say done.", "--model", "haiku", "--setting-sources", "project", "--no-session-persistence", "--allowedTools", "Read", "--output-format", "text"}, extra...)
	cmd := exec.CommandContext(ctx, claude, args...)
	cmd.Dir = work
	cmd.Stdin = nil
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("claude: %v\n%s", err, out)
	}
	evs, err := (&agentevents.Spool{Dir: filepath.Join(ws, "events")}).Read("real-1")
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, ev := range evs {
		kinds = append(kinds, string(ev.Kind))
		if ev.SessionID != "real-1" || ev.Process.Provider != "claude" {
			t.Fatalf("envelope = %+v", ev)
		}
	}
	got := strings.Join(kinds, " ")
	for _, want := range []string{"session.ready", "turn.started", "agent.tool_use", "agent.tool_result", "turn.completed", "session.idle", "process.exited"} {
		if !strings.Contains(got, want) {
			t.Fatalf("spool kinds %q lack %s", got, want)
		}
	}
	if raw, _ := os.ReadFile(filepath.Join(ws, "events", "real-1.jsonl")); strings.Contains(string(raw), "hello from the real test") || strings.Contains(string(raw), "Read hello.txt") {
		t.Fatal("the spool carries prompt or file content")
	}
	if b, _ := os.ReadFile(fired); !strings.Contains(string(b), "fired") {
		t.Fatal("the project's own Stop hook did not fire next to the planted hooks")
	}
}
