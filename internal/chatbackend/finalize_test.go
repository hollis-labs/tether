package chatbackend

import (
	"encoding/json"
	"testing"

	"github.com/hollis-labs/libs/ui/ui-go/chatstream"
)

func ev(verb chatstream.Verb, part string, f func(*chatstream.Event)) chatstream.Event {
	e := chatstream.Event{V: chatstream.SchemaVersion, Verb: verb, PartID: part}
	if f != nil {
		f(&e)
	}
	return e
}

func TestFinalizerFillsFinals(t *testing.T) {
	in := []chatstream.Event{
		ev(chatstream.VerbPartStart, "t", func(e *chatstream.Event) { e.Kind = "text" }),
		ev(chatstream.VerbPartDelta, "t", func(e *chatstream.Event) { e.Text = "hel" }),
		ev(chatstream.VerbPartDelta, "t", func(e *chatstream.Event) { e.Text = "lo" }),
		ev(chatstream.VerbPartEnd, "t", nil),
		ev(chatstream.VerbPartStart, "r", func(e *chatstream.Event) { e.Kind = "reasoning" }),
		ev(chatstream.VerbPartDelta, "r", func(e *chatstream.Event) { e.Text = "hmm" }),
		ev(chatstream.VerbPartEnd, "r", func(e *chatstream.Event) { e.Final = json.RawMessage(`{"signature":"s"}`) }),
		ev(chatstream.VerbPartStart, "c", func(e *chatstream.Event) { e.Kind = "tool_call" }),
		ev(chatstream.VerbPartDelta, "c", func(e *chatstream.Event) { e.JSONFragment = `{"a":` }),
		ev(chatstream.VerbPartDelta, "c", func(e *chatstream.Event) { e.JSONFragment = `1}` }),
		ev(chatstream.VerbPartEnd, "c", nil),
		ev(chatstream.VerbPartStart, "x", func(e *chatstream.Event) {
			e.Kind = "tool_result"
			e.Meta = map[string]json.RawMessage{chatstream.MetaIsError: json.RawMessage("true")}
		}),
		ev(chatstream.VerbPartEnd, "x", func(e *chatstream.Event) {
			e.Final = json.RawMessage(`{"type":"tool_result","content":[{"type":"text","text":"a"},{"type":"image"},{"type":"text","text":"b"}]}`)
		}),
	}
	for _, drop := range []bool{false, true} {
		f := NewFinalizer(FinalizerOptions{DropDeltas: drop})
		var out []chatstream.Event
		for _, e := range in {
			out = append(out, f.Push(e)...)
		}
		ends := map[string]string{}
		deltas := 0
		for _, e := range out {
			if e.Verb == chatstream.VerbPartEnd {
				ends[e.PartID] = string(e.Final)
			}
			if e.Verb == chatstream.VerbPartDelta {
				deltas++
			}
		}
		if drop && deltas != 0 || !drop && deltas != 5 {
			t.Errorf("drop=%v: %d deltas", drop, deltas)
		}
		want := map[string]string{
			"t": `{"text":"hello"}`,
			"r": `{"signature":"s","text":"hmm"}`,
			"c": `{"a":1}`,
			"x": `{"content":[{"type":"text","text":"a"},{"type":"image"},{"type":"text","text":"b"}],"is_error":true,"text":"a\nb","type":"tool_result"}`,
		}
		for id, w := range want {
			if ends[id] != w {
				t.Errorf("drop=%v part %s final = %s, want %s", drop, id, ends[id], w)
			}
		}
	}
}

func TestFinalizerToolCallWithoutArgs(t *testing.T) {
	f := NewFinalizer(FinalizerOptions{})
	f.Push(ev(chatstream.VerbPartStart, "c", func(e *chatstream.Event) { e.Kind = "tool_call" }))
	out := f.Push(ev(chatstream.VerbPartEnd, "c", nil))
	if string(out[0].Final) != "{}" {
		t.Errorf("final = %s", out[0].Final)
	}
}

func TestStreamEndsAfterTerminal(t *testing.T) {
	s := NewStream("r1", nil)
	s.Emit(chatstream.Event{Verb: chatstream.VerbRunStart})
	go s.Emit(chatstream.Event{Verb: chatstream.VerbRunAbort}, chatstream.Event{Verb: chatstream.VerbRunFinish})
	var verbs []chatstream.Verb
	for e, err := range s.Events() {
		if err != nil {
			t.Fatal(err)
		}
		if e.RunID != "r1" {
			t.Errorf("run id %q", e.RunID)
		}
		verbs = append(verbs, e.Verb)
	}
	if len(verbs) != 2 || verbs[1] != chatstream.VerbRunAbort {
		t.Errorf("verbs = %v", verbs)
	}
	for range s.Events() {
		t.Error("second consumer got events")
	}
}
