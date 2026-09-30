package opencode

import (
	"bufio"
	"os"
	"path/filepath"
	"slices"
	"testing"

	llmtypes "github.com/hollis-labs/go-llm-types"
)

// The testdata/run_*.jsonl fixtures are verbatim `opencode run --format json`
// stdout captured from opencode 1.18.30: turn 1 opens a session, turn 2
// resumes it with `--session <id>` and recalls turn 1's content, and the
// tool-use turn exercises the tool_use event shape.

func parseFixture(t *testing.T, name string) []llmtypes.StreamEvent {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []llmtypes.StreamEvent
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		evs, err := cliAdapter{}.ParseLine(sc.Bytes())
		if err != nil {
			t.Fatalf("ParseLine: %v", err)
		}
		out = append(out, evs...)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func sessionIDs(evs []llmtypes.StreamEvent) []string {
	var ids []string
	for _, ev := range evs {
		if ev.Type == llmtypes.EventSessionID && !slices.Contains(ids, ev.SessionID) {
			ids = append(ids, ev.SessionID)
		}
	}
	return ids
}

func TestParseLine_ResumedTurnKeepsSessionID(t *testing.T) {
	turn1 := sessionIDs(parseFixture(t, "run_turn1.jsonl"))
	turn2 := sessionIDs(parseFixture(t, "run_turn2_resume.jsonl"))
	if len(turn1) != 1 || turn1[0] == "" {
		t.Fatalf("turn 1 session ids = %v; want exactly one", turn1)
	}
	if !slices.Equal(turn1, turn2) {
		t.Errorf("resumed turn session ids = %v; want %v", turn2, turn1)
	}
}

func TestParseLine_ForwardsEveryLineAsDelta(t *testing.T) {
	for _, name := range []string{"run_turn1.jsonl", "run_turn2_resume.jsonl", "run_tool_use.jsonl"} {
		evs := parseFixture(t, name)
		counts := map[llmtypes.EventType]int{}
		for _, ev := range evs {
			counts[ev.Type]++
		}
		deltas, ids := counts[llmtypes.EventDelta], counts[llmtypes.EventSessionID]
		// Every opencode JSON line carries a top-level sessionID, so each
		// line yields one raw delta plus one session-id event.
		if deltas == 0 || deltas != ids {
			t.Errorf("%s: deltas=%d session-id events=%d; want equal and non-zero", name, deltas, ids)
		}
	}
}

func TestParseLine_NonJSONStillForwarded(t *testing.T) {
	evs, err := cliAdapter{}.ParseLine([]byte("Error: Session not found"))
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Type != llmtypes.EventDelta {
		t.Errorf("events = %+v; want one delta", evs)
	}
}

func TestBuildArgs_ResumeFlag(t *testing.T) {
	if got := (cliAdapter{}).BuildArgs("hi", "", ""); !slices.Equal(got, []string{"--format", "json", "hi"}) {
		t.Errorf("first turn args = %q", got)
	}
	want := []string{"--format", "json", "--session", "ses_x", "hi"}
	if got := (cliAdapter{}).BuildArgs("hi", "", "ses_x"); !slices.Equal(got, want) {
		t.Errorf("resume args = %q; want %q", got, want)
	}
}
