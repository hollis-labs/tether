package agentevents

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The one suite for the recorder both `tether hook` and `tether-worker hook` run. The cmd packages only test their wiring.

func realHook(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "claude", "real", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestRecordClaudeHookRecordsASequence(t *testing.T) {
	spool := t.TempDir()
	for _, name := range []string{"SessionStart", "UserPromptSubmit", "PreToolUse-read", "PostToolUse-read", "Stop"} {
		if class := RecordClaudeHook(context.Background(), strings.NewReader(realHook(t, name)), "sess-1", spool); class != "" {
			t.Fatalf("%s: class %q", name, class)
		}
	}
	evs, err := (&Spool{Dir: spool}).Read("sess-1")
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for i, ev := range evs {
		kinds = append(kinds, string(ev.Kind))
		if ev.SessionID != "sess-1" || ev.Process.Provider != "claude" || ev.Process.Runtime != "hooks" || ev.Process.ProviderSessionID == "" || ev.Sequence != uint64(i+1) {
			t.Fatalf("envelope %d = %+v", i, ev)
		}
	}
	want := "session.ready turn.started session.processing agent.tool_use agent.tool_result turn.completed session.idle"
	if got := strings.Join(kinds, " "); got != want {
		t.Fatalf("kinds = %q, want %q", got, want)
	}
}

func TestRecordClaudeHookEveryFailureIsAClassAndWritesNothing(t *testing.T) {
	spool := t.TempDir()
	unwritable := filepath.Join(t.TempDir(), "ro")
	if err := os.Mkdir(unwritable, 0o500); err != nil {
		t.Fatal(err)
	}
	good := realHook(t, "PreToolUse-read")
	for _, c := range []struct{ name, stdin, session, spool, want string }{
		{"no session: not a managed session", good, "", spool, ""},
		{"no spool", good, "s", "", ""},
		{"relative spool", good, "s", "events", "spool_not_absolute"},
		{"not json", "not json with a secret-token inside", "s", spool, "payload_invalid"},
		{"empty stdin", "", "s", spool, "payload_invalid"},
		{"unknown event is no event", `{"hook_event_name":"FutureEvent"}`, "s", spool, ""},
		{"too large", strings.Repeat("x", MaxHookPayload+10), "s", spool, "payload_too_large"},
		{"unsafe session id", good, "../x", spool, "record_failed"},
		{"unwritable spool", good, "s", filepath.Join(unwritable, "deeper"), "record_failed"},
	} {
		if class := RecordClaudeHook(context.Background(), strings.NewReader(c.stdin), c.session, c.spool); class != c.want {
			t.Fatalf("%s: class %q, want %q", c.name, class, c.want)
		}
	}
	if entries, _ := os.ReadDir(spool); len(entries) != 0 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		// Only the drop counter of a failed record may exist; no event file.
		for _, n := range names {
			if strings.HasSuffix(n, ".jsonl") {
				t.Fatalf("a refused payload wrote events: %v", names)
			}
		}
	}
}

func TestRecordClaudeHookWritesNoContent(t *testing.T) {
	spool := t.TempDir()
	for _, name := range []string{"UserPromptSubmit", "PostToolUse-read"} {
		if class := RecordClaudeHook(context.Background(), strings.NewReader(realHook(t, name)), "s", spool); class != "" {
			t.Fatal(class)
		}
	}
	raw, _ := os.ReadFile(filepath.Join(spool, "s.jsonl"))
	for _, content := range []string{"hello from the capture", "Read the file hello.txt"} {
		if strings.Contains(string(raw), content) {
			t.Fatalf("the spool carries content %q", content)
		}
	}
	if info, _ := os.Stat(filepath.Join(spool, "s.jsonl")); info.Mode().Perm() != 0o600 {
		t.Fatalf("spool file mode %v", info.Mode().Perm())
	}
}
