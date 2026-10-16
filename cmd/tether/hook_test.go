package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/agentevents"
)

// The recorder has its own suite in internal/agentevents; this is the command's wiring: flags in, silence out, exit 0.
func runHook(t *testing.T, stdin string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	cmd := newHookCommand()
	var out, errb bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetArgs(args)
	err = cmd.ExecuteContext(context.Background())
	return out.String(), errb.String(), err
}

func TestHookClaudeCommandWiring(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "internal", "agentevents", "testdata", "claude", "real", "UserPromptSubmit.json"))
	if err != nil {
		t.Fatal(err)
	}
	spool := t.TempDir()
	if stdout, stderr, err := runHook(t, string(raw), "claude", "--session", "c1", "--spool", spool); err != nil || stdout != "" || stderr != "" {
		t.Fatalf("err=%v stdout=%q stderr=%q (observe-only means silent success)", err, stdout, stderr)
	}
	if evs, err := (&agentevents.Spool{Dir: spool}).Read("c1"); err != nil || len(evs) != 2 {
		t.Fatalf("spooled %d err=%v", len(evs), err)
	}
	// A failure is one class on stderr and still exit 0.
	stdout, stderr, err := runHook(t, "not json", "claude", "--session", "c1", "--spool", spool)
	if err != nil || stdout != "" || strings.TrimSpace(stderr) != "tether hook: payload_invalid" {
		t.Fatalf("err=%v stdout=%q stderr=%q", err, stdout, stderr)
	}
}
