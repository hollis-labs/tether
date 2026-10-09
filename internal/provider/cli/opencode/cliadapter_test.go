package opencode

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/hollis-labs/substrate/harness/adapters/agentsessions"
	gop "github.com/hollis-labs/substrate/harness/adapters/provider"
	llmtypes "github.com/hollis-labs/substrate/llm-core/llmtypes"

	"github.com/hollis-labs/tether/internal/launch"
)

// fakeOpencode writes a script standing in for `opencode run --format json`.
// It appends its argv to argv.log, fails a resume of ses_dead the way
// opencode does ("Session not found" on stderr, no JSON, exit 1), and
// otherwise prints one step: step_start, text, step_finish with usage.
func fakeOpencode(t *testing.T, dir string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("test script needs sh")
	}
	path := filepath.Join(dir, "opencode")
	body := `#!/bin/sh
printf '%s\n' "$*" >> "` + filepath.Join(dir, "argv.log") + `"
sid=ses_fresh
while [ $# -gt 0 ]; do
  if [ "$1" = "--session" ]; then sid=$2; fi
  shift
done
if [ "$sid" = "ses_dead" ]; then
  printf 'Error: Session not found\n' 1>&2
  exit 1
fi
printf '{"type":"step_start","sessionID":"%s","part":{"type":"step-start"}}\n' "$sid"
printf '{"type":"text","sessionID":"%s","part":{"type":"text","text":"hi"}}\n' "$sid"
printf '{"type":"step_finish","sessionID":"%s","part":{"type":"step-finish","reason":"stop","tokens":{"input":3,"output":5,"reasoning":0,"cache":{"read":7,"write":11}}}}\n' "$sid"
`
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func argvLog(t *testing.T, dir string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "argv.log"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func startFake(t *testing.T, preset string) (agentsessions.Session, chan llmtypes.StreamEvent, string) {
	t.Helper()
	// Catalogs seeded before the adapter owned the subcommand declare
	// `args: [run]`; New must not double it.
	return startFakeWithArgs(t, preset, []string{"run"})
}

func startFakeWithArgs(t *testing.T, preset string, args []string) (agentsessions.Session, chan llmtypes.StreamEvent, string) {
	t.Helper()
	dir := t.TempDir()
	rt, err := New(&launch.Plan{Command: fakeOpencode(t, dir), Args: args})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	events := make(chan llmtypes.StreamEvent, 64)
	sess, err := rt.Start(context.Background(), agentsessions.StartOptions{
		Workdir:         dir,
		LogPath:         filepath.Join(dir, "session.log"),
		SessionIDPreset: preset,
		EventFanout:     events,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = sess.Stop(context.Background()) })
	return sess, events, dir
}

func drain(ch chan llmtypes.StreamEvent) []llmtypes.StreamEvent {
	var out []llmtypes.StreamEvent
	for {
		select {
		case ev := <-ch:
			out = append(out, ev)
		default:
			return out
		}
	}
}

func TestRuntime_TypedEventsAndResumeArgv(t *testing.T) {
	sess, events, dir := startFake(t, "")
	for _, prompt := range []string{"one", "two"} {
		if err := sess.SendInput(context.Background(), []byte(prompt)); err != nil {
			t.Fatalf("SendInput(%s): %v", prompt, err)
		}
	}

	want := []string{
		"run --format json --agent tether-agent -- one",
		"run --format json --agent tether-agent --session ses_fresh -- two",
	}
	if got := argvLog(t, dir); !slices.Equal(got, want) {
		t.Errorf("argv per turn = %q; want %q", got, want)
	}

	var types []llmtypes.EventType
	var usage *llmtypes.Usage
	for _, ev := range drain(events) {
		types = append(types, ev.Type)
		if ev.Type == llmtypes.EventUsage {
			usage = ev.Usage
		}
	}
	turn := []llmtypes.EventType{llmtypes.EventSessionID, llmtypes.EventDelta, llmtypes.EventUsage, llmtypes.EventDone}
	if !slices.Equal(types, append(slices.Clone(turn), turn...)) {
		t.Errorf("event types = %v; want %v twice", types, turn)
	}
	if usage == nil || *usage != (llmtypes.Usage{InputTokens: 3, OutputTokens: 5, CacheReadTokens: 7, CacheCreationTokens: 11, StopReason: "end_turn"}) {
		t.Errorf("usage = %+v", usage)
	}
}

// The current seed declares `args: []`; the argv matches the legacy
// `args: [run]` catalog above.
func TestRuntime_SeedArgsEmpty(t *testing.T) {
	sess, _, dir := startFakeWithArgs(t, "", nil)
	if err := sess.SendInput(context.Background(), []byte("one")); err != nil {
		t.Fatalf("SendInput: %v", err)
	}
	if got, want := argvLog(t, dir), []string{"run --format json --agent tether-agent -- one"}; !slices.Equal(got, want) {
		t.Errorf("argv = %q; want %q", got, want)
	}
}

func TestRuntime_LostSessionIsTypedAndNextTurnStartsFresh(t *testing.T) {
	sess, events, dir := startFake(t, "ses_dead")

	err := sess.SendInput(context.Background(), []byte("turn N"))
	if !errors.Is(err, gop.ErrProviderSessionLost) {
		t.Fatalf("turn N err = %v; want ErrProviderSessionLost", err)
	}
	evs := drain(events)
	if len(evs) != 1 || evs[0].Type != llmtypes.EventError || !strings.Contains(evs[0].Error, gop.ErrProviderSessionLost.Error()) {
		t.Errorf("turn N events = %+v; want one EventError naming the lost session", evs)
	}

	if err := sess.SendInput(context.Background(), []byte("turn N+1")); err != nil {
		t.Fatalf("turn N+1: %v", err)
	}
	if got := sess.(agentsessions.SessionIDer).ProviderSessionID(); got != "ses_fresh" {
		t.Errorf("session id after turn N+1 = %q; want ses_fresh", got)
	}
	log := argvLog(t, dir)
	if len(log) != 2 || !strings.Contains(log[0], "--session ses_dead") || strings.Contains(log[1], "--session") {
		t.Errorf("argv per turn = %q; want a resume of ses_dead, then no --session", log)
	}
}

func TestPlanWithoutRunSubcommand(t *testing.T) {
	cases := []struct{ in, want []string }{
		{[]string{"run"}, []string{}},
		{[]string{"run", "--pure"}, []string{"--pure"}},
		{nil, nil},
		{[]string{"--pure"}, []string{"--pure"}},
	}
	for _, c := range cases {
		plan := &launch.Plan{Args: c.in}
		got := planWithoutRunSubcommand(plan).Args
		if !slices.Equal(got, c.want) {
			t.Errorf("args %q -> %q; want %q", c.in, got, c.want)
		}
		if len(c.in) > 0 && c.in[0] == "run" && plan.Args[0] != "run" {
			t.Errorf("input plan mutated: %q", plan.Args)
		}
	}
}
