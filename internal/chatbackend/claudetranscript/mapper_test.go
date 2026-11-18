package claudetranscript

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func lines(s ...string) [][]byte {
	out := make([][]byte, len(s))
	for i, l := range s {
		out[i] = []byte(l)
	}
	return out
}

func push(m *Mapper, ls [][]byte) []Signal {
	var out []Signal
	for _, l := range ls {
		out = append(out, m.Push(l)...)
	}
	return out
}

func kinds(sigs []Signal) string {
	var b strings.Builder
	for _, s := range sigs {
		switch s.Kind {
		case TurnStart:
			b.WriteString("S ")
		case TurnFrame:
			var w struct {
				Type    string `json:"type"`
				Message struct {
					ID string `json:"id"`
				} `json:"message"`
			}
			_ = json.Unmarshal(s.Frame, &w)
			if w.Type == "assistant" {
				b.WriteString("A:" + w.Message.ID + " ")
			} else {
				b.WriteString("R ")
			}
		case TurnEnd:
			b.WriteString("E:" + string(s.End) + " ")
		case TurnQueued:
			b.WriteString("Q ")
		}
	}
	return strings.TrimSpace(b.String())
}

// Claude Code writes a tool's result right after its tool_use block, before the same message's next block. The mapper
// holds results back so each message's blocks stay together.
func TestToolResultsWaitForTheirMessage(t *testing.T) {
	var m Mapper
	sigs := push(&m, lines(
		`{"type":"attachment","attachment":{"type":"x"}}`,
		`{"type":"assistant","message":{"id":"m0","content":[{"type":"text","text":"before any prompt"}]}}`,
		`{"type":"user","promptId":"p1","message":{"content":"hi"},"promptSource":"typed","turnOrigin":"human"}`,
		`{"type":"assistant","message":{"id":"m1","content":[{"type":"tool_use","id":"a","name":"Read","input":{}}]}}`,
		`{"type":"user","promptId":"p1","message":{"content":[{"type":"tool_result","tool_use_id":"a","content":"x"}]}}`,
		`{"type":"assistant","message":{"id":"m1","content":[{"type":"tool_use","id":"b","name":"Read","input":{}}]}}`,
		`{"type":"user","promptId":"p1","message":{"content":[{"type":"tool_result","tool_use_id":"b","content":"y"}]}}`,
		`{"type":"user","promptId":"p1","isMeta":true,"message":{"content":[{"type":"text","text":"skill body"}]}}`,
		`{"type":"assistant","isSidechain":true,"message":{"id":"sub","content":[{"type":"text","text":"subagent"}]}}`,
		`{"type":"assistant","message":{"id":"m2","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":2}}}`,
		`{"type":"system","subtype":"stop_hook_summary"}`,
		`{"type":"system","subtype":"turn_duration"}`,
		`{"type":"assistant","message":{"id":"m3","content":[{"type":"text","text":"late line of a closed turn"}]}}`,
	))
	if got, want := kinds(sigs), "S A:m1 A:m1 R R A:m2 E:completed"; got != want {
		t.Errorf("signals = %s\nwant      %s", got, want)
	}
	if sigs[0].Prompt != (Prompt{Text: "hi", Source: "typed", Origin: "human"}) {
		t.Errorf("prompt = %+v", sigs[0].Prompt)
	}
	end := sigs[len(sigs)-1]
	if !strings.Contains(string(end.Frame), `"stop_reason":"end_turn"`) || !strings.Contains(string(end.Frame), `"output_tokens":2`) {
		t.Errorf("result frame = %s", end.Frame)
	}
}

func TestInterruptAndSupersede(t *testing.T) {
	var m Mapper
	sigs := push(&m, lines(
		`{"type":"user","promptId":"p1","message":{"content":"one"}}`,
		`{"type":"assistant","message":{"id":"m1","content":[{"type":"tool_use","id":"a","name":"Bash","input":{}}]}}`,
		`{"type":"user","promptId":"p1","message":{"content":[{"type":"text","text":"[Request interrupted by user for tool use]"}]}}`,
		`{"type":"user","promptId":"p2","message":{"content":"two"}}`,
		`{"type":"attachment","attachment":{"type":"queued_command","prompt":"three"}}`,
		`{"type":"user","promptId":"p4","message":{"content":"four"}}`,
	))
	if got, want := kinds(sigs), "S A:m1 E:interrupted S Q E:superseded S"; got != want {
		t.Errorf("signals = %s\nwant      %s", got, want)
	}
	if !strings.Contains(string(sigs[2].Frame), `"aborted_tools"`) {
		t.Errorf("interrupt frame = %s", sigs[2].Frame)
	}
	if sigs[4].Text != "three" {
		t.Errorf("queued = %q", sigs[4].Text)
	}
	if got := kinds(m.End(EndForced)); got != "E:forced" {
		t.Errorf("End = %s", got)
	}
	if m.End(EndForced) != nil {
		t.Error("End with no open turn should be nil")
	}
}

func TestLocate(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "-some-project", "abc-123.jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Locate(dir, "abc-123"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("before the file exists: %v", err)
	}
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := Locate(dir, "abc-123"); err != nil || got != p {
		t.Fatalf("Locate = %q, %v", got, err)
	}
	if _, err := Locate(dir, "../x"); err == nil {
		t.Fatal("an unsafe id must be refused")
	}
}
