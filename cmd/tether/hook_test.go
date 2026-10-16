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

func hookFixture(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "internal", "agentevents", "testdata", "claude", "real", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestHookClaudeRecordsToTheSpoolAndSaysNothing(t *testing.T) {
	spool := t.TempDir()
	for _, name := range []string{"SessionStart", "UserPromptSubmit", "PreToolUse-read", "PostToolUse-read", "Stop"} {
		stdout, stderr, err := runHook(t, hookFixture(t, name), "claude", "--session", "sess-1", "--spool", spool)
		if err != nil || stdout != "" || stderr != "" {
			t.Fatalf("%s: err=%v stdout=%q stderr=%q (observe-only means silent success)", name, err, stdout, stderr)
		}
	}
	evs, err := (&agentevents.Spool{Dir: spool}).Read("sess-1")
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, ev := range evs {
		kinds = append(kinds, string(ev.Kind))
		if ev.SessionID != "sess-1" || ev.Process.Provider != "claude" || ev.Process.Runtime != "hooks" || ev.Process.ProviderSessionID == "" {
			t.Fatalf("envelope = %+v", ev)
		}
	}
	want := "session.ready turn.started session.processing agent.tool_use agent.tool_result turn.completed session.idle"
	if got := strings.Join(kinds, " "); got != want {
		t.Fatalf("kinds = %q, want %q", got, want)
	}
	for i, ev := range evs {
		if ev.Sequence != uint64(i+1) {
			t.Fatalf("sequence %d at %d", ev.Sequence, i)
		}
	}
}

// Every way a hook can go wrong must still exit 0 and print nothing to stdout: the agent treats exit 2 or a JSON decision on
// stdout as an instruction, and a spool problem is never a reason to hold the agent.
func TestHookClaudeFailsOpenOnEverything(t *testing.T) {
	spool := t.TempDir()
	unwritable := filepath.Join(t.TempDir(), "ro")
	if err := os.Mkdir(unwritable, 0o500); err != nil {
		t.Fatal(err)
	}
	good := hookFixture(t, "PreToolUse-read")
	for name, c := range map[string]struct {
		stdin string
		args  []string
	}{
		"no flags (not a managed session)": {good, []string{"claude"}},
		"no spool":                         {good, []string{"claude", "--session", "s"}},
		"not json":                         {"not json", []string{"claude", "--session", "s", "--spool", spool}},
		"empty stdin":                      {"", []string{"claude", "--session", "s", "--spool", spool}},
		"unknown event":                    {`{"hook_event_name":"FutureEvent"}`, []string{"claude", "--session", "s", "--spool", spool}},
		"unsafe session id":                {good, []string{"claude", "--session", "../x", "--spool", spool}},
		"unwritable spool":                 {good, []string{"claude", "--session", "s", "--spool", filepath.Join(unwritable, "deeper")}},
	} {
		stdout, _, err := runHook(t, c.stdin, c.args...)
		if err != nil || stdout != "" {
			t.Fatalf("%s: err=%v stdout=%q", name, err, stdout)
		}
	}
	if entries, _ := os.ReadDir(spool); len(entries) != 0 {
		t.Fatalf("a refused payload wrote to the spool: %v", entries)
	}
}

func TestHookClaudeWritesNoContent(t *testing.T) {
	spool := t.TempDir()
	if _, _, err := runHook(t, hookFixture(t, "UserPromptSubmit"), "claude", "--session", "s", "--spool", spool); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runHook(t, hookFixture(t, "PostToolUse-read"), "claude", "--session", "s", "--spool", spool); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(spool, "s.jsonl"))
	for _, content := range []string{"hello from the capture", "Read the file hello.txt"} {
		if strings.Contains(string(raw), content) {
			t.Fatalf("the spool carries content %q", content)
		}
	}
	info, _ := os.Stat(filepath.Join(spool, "s.jsonl"))
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("spool file mode %v", info.Mode().Perm())
	}
}
