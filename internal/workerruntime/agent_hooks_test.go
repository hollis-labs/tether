package workerruntime

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/agentevents"
	"github.com/hollis-labs/tether/internal/workerwire"
)

func hookBinary(t *testing.T, mode os.FileMode) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "tether-worker")
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAgentHooksSettingsShape(t *testing.T) {
	raw, err := agentHooksSettings("/opt/w/tether-worker", "sess-1", "/ws/sess-1/events")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Hooks map[string][]struct {
			Matcher string `json:"matcher"`
			Hooks   []struct{ Type, Command string }
		}
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	want := "'/opt/w/tether-worker' hook claude --session 'sess-1' --spool '/ws/sess-1/events'"
	if len(doc.Hooks) != len(claudeHookEvents) {
		t.Fatalf("events planted = %d, want %d", len(doc.Hooks), len(claudeHookEvents))
	}
	for _, event := range claudeHookEvents {
		groups := doc.Hooks[event]
		if len(groups) != 1 || len(groups[0].Hooks) != 1 || groups[0].Matcher != "" || groups[0].Hooks[0].Type != "command" || groups[0].Hooks[0].Command != want {
			t.Fatalf("%s hook = %+v", event, groups)
		}
	}
	if strings.Contains(string(raw), "token") || strings.Contains(string(raw), "TETHER_") {
		t.Fatal("the planted settings mention a token or environment")
	}
}

// Every event the mapper knows is planted, so a mapped event can never silently go unobserved.
func TestPlantedEventsCoverEveryEventTheMapperKnows(t *testing.T) {
	planted := map[string]bool{}
	for _, e := range claudeHookEvents {
		planted[e] = true
	}
	for _, e := range []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse", "PostToolUseFailure", "PermissionRequest", "PermissionDenied", "Notification", "SubagentStart", "SubagentStop", "Stop", "SessionEnd"} {
		if obs, _ := agentevents.ClaudeHook([]byte(`{"hook_event_name":"` + e + `","notification_type":"permission_prompt"}`)); len(obs) == 0 {
			t.Fatalf("mapper does not know %s", e)
		}
		if !planted[e] {
			t.Fatalf("%s is mapped but not planted", e)
		}
	}
}

func TestAgentHooksSettingsRefusesUnsafeArguments(t *testing.T) {
	for name, c := range map[string]struct{ binary, session, spool string }{
		"session with a quote": {"/b", "a'b", "/s"},
		"session with a path":  {"/b", "../x", "/s"},
		"empty session":        {"/b", "", "/s"},
		"relative spool":       {"/b", "s", "events"},
		"spool with a quote":   {"/b", "s", "/s/'x"},
		"spool with a newline": {"/b", "s", "/s\n/x"},
		"empty spool":          {"/b", "s", ""},
	} {
		if _, err := agentHooksSettings(c.binary, c.session, c.spool); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

func TestCheckHookBinary(t *testing.T) {
	good := hookBinary(t, 0o755)
	if err := checkHookBinary(good); err != nil {
		t.Fatalf("good binary: %v", err)
	}
	for name, p := range map[string]string{
		"group-writable": hookBinary(t, 0o775),
		"other-writable": hookBinary(t, 0o757),
		"relative":       "tether-worker",
		"missing":        filepath.Join(t.TempDir(), "nope"),
		"quoted":         filepath.Join(t.TempDir(), "it's"),
	} {
		if err := checkHookBinary(p); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	writableDir := t.TempDir()
	if err := os.Chmod(writableDir, 0o777); err != nil {
		t.Fatal(err)
	}
	inWritable := filepath.Join(writableDir, "tether-worker")
	if err := os.WriteFile(inWritable, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := checkHookBinary(inWritable); err == nil {
		t.Fatal("a binary in an other-writable directory was accepted")
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(good, link); err != nil {
		t.Fatal(err)
	}
	if err := checkHookBinary(link); err == nil {
		t.Fatal("a symlink was accepted")
	}
}

func TestAgentHookArgsPlantsPrivatelyAndOnlyWhenAsked(t *testing.T) {
	ws := t.TempDir()
	e := &Executor{Binary: hookBinary(t, 0o755)}
	if got := e.agentHookArgs(ws, workerwire.LaunchSpec{Session: "s1"}); got != nil {
		t.Fatalf("hooks planted without the flag: %v", got)
	}
	if _, err := os.Stat(filepath.Join(ws, "events")); !os.IsNotExist(err) {
		t.Fatal("events directory created without the flag")
	}
	got := e.agentHookArgs(ws, workerwire.LaunchSpec{Session: "s1", AgentEvents: true})
	settings := filepath.Join(ws, "agent-hooks.json")
	if len(got) != 2 || got[0] != "--settings" || got[1] != settings {
		t.Fatalf("args = %v", got)
	}
	for path, mode := range map[string]os.FileMode{filepath.Join(ws, "events"): 0o700, settings: 0o600} {
		fi, err := os.Stat(path)
		if err != nil || fi.Mode().Perm() != mode {
			t.Fatalf("%s: %v mode %v, want %v", path, err, fi, mode)
		}
	}
	raw, _ := os.ReadFile(settings)
	if !strings.Contains(string(raw), "--spool '"+filepath.Join(ws, "events")+"'") {
		t.Fatalf("settings do not point at the session's events dir: %s", raw)
	}
}

// A refusal launches without hooks: no args, no error to the caller.
func TestAgentHookArgsFailsOpen(t *testing.T) {
	ws := t.TempDir()
	for name, e := range map[string]*Executor{
		"writable binary": {Binary: hookBinary(t, 0o777)},
		"relative binary": {Binary: "tether-worker"},
	} {
		if got := e.agentHookArgs(ws, workerwire.LaunchSpec{Session: "s1", AgentEvents: true}); got != nil {
			t.Fatalf("%s: planted %v", name, got)
		}
	}
	if got := (&Executor{Binary: hookBinary(t, 0o755)}).agentHookArgs(ws, workerwire.LaunchSpec{Session: "../x", AgentEvents: true}); got != nil {
		t.Fatalf("unsafe session id planted %v", got)
	}
	if _, err := os.Stat(filepath.Join(ws, "agent-hooks.json")); !os.IsNotExist(err) {
		t.Fatal("a settings file was written for a refused launch")
	}
}
