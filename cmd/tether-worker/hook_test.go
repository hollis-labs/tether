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

func runWorkerHook(t *testing.T, stdin string, args ...string) (stdout, stderr string, err error) {
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

func TestWorkerHookRecordsSilentlyAndFailsOpen(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "internal", "agentevents", "testdata", "claude", "real", "UserPromptSubmit.json"))
	if err != nil {
		t.Fatal(err)
	}
	spool := t.TempDir()
	stdout, stderr, err := runWorkerHook(t, string(raw), "claude", "--session", "w1", "--spool", spool)
	if err != nil || stdout != "" || stderr != "" {
		t.Fatalf("err=%v stdout=%q stderr=%q", err, stdout, stderr)
	}
	evs, err := (&agentevents.Spool{Dir: spool}).Read("w1")
	if err != nil || len(evs) != 2 || evs[0].Kind != "turn.started" {
		t.Fatalf("spooled %+v err=%v", evs, err)
	}
	for name, c := range map[string][]string{
		"no flags":      {"claude"},
		"relative path": {"claude", "--session", "w1", "--spool", "events"},
		"bad session":   {"claude", "--session", "../x", "--spool", spool},
	} {
		if stdout, _, err := runWorkerHook(t, string(raw), c...); err != nil || stdout != "" {
			t.Fatalf("%s: err=%v stdout=%q", name, err, stdout)
		}
	}
	if _, stderr, err := runWorkerHook(t, "not json", "claude", "--session", "w1", "--spool", spool); err != nil || strings.TrimSpace(stderr) != "tether-worker hook: payload_invalid" {
		t.Fatalf("invalid payload: err=%v stderr=%q", err, stderr)
	}
}
