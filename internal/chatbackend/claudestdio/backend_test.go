package claudestdio

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/libs/ui/ui-go/chatstream"

	"github.com/hollis-labs/tether/internal/chatbackend"
)

const (
	turnsText1 = "Read hello.txt and other.txt (both at once, in parallel). Then work out carefully which code has more letters, thinking it through before you answer in one sentence."
	doneText   = "Reply with only the word: done"
)

// Turn 1 is submitted (two parallel tool calls, thinking, text); turn 2 is written by another writer and comes out as
// an observed run with its text.
func TestTurnsGolden(t *testing.T) {
	fx := loadFixture(t, "turns")
	h := newHarness(t, nil)
	h.replay(fx, nil, nil)
	h.settle()
	got := h.golden("turns")

	if len(h.runs) != 2 {
		t.Fatalf("runs = %d, want 2", len(h.runs))
	}
	r1, r2 := h.runs[0], h.runs[1]
	if r1.Observed || !r2.Observed {
		t.Fatalf("observed flags = %v %v, want false true", r1.Observed, r2.Observed)
	}
	if r2.Text != doneText || r2.Source != SourceStdin || r2.Origin != "send_turn" || r2.TurnRef == "" {
		t.Errorf("observed run = %+v", *r2)
	}

	e1 := got[r1.ID]
	if tv := terminal(e1); tv.Verb != chatstream.VerbRunFinish || tv.Reason != string(chatstream.FinishStop) {
		t.Errorf("turn 1 ends with %s %s", tv.Verb, tv.Reason)
	}
	if e1[0].Verb != chatstream.VerbRunStart || e1[0].Model != "claude-sonnet-5-5" {
		t.Errorf("turn 1 starts with %s model %q", e1[0].Verb, e1[0].Model)
	}
	f1 := finals(e1)
	if len(f1["tool_call"]) != 2 || len(f1["tool_result"]) != 2 || len(f1["reasoning"]) != 1 || len(f1["text"]) != 1 {
		t.Fatalf("turn 1 parts: %d tool_call, %d tool_result, %d reasoning, %d text", len(f1["tool_call"]), len(f1["tool_result"]), len(f1["reasoning"]), len(f1["text"]))
	}
	if txt, _ := f1["text"][0]["text"].(string); !strings.Contains(txt, "ORANGE-42") {
		t.Errorf("turn 1 text final = %q", txt)
	}
	// Claude Code streams thinking as token estimates only (thinking_display "updates"): the text is empty, the
	// signature is there.
	if _, ok := f1["reasoning"][0]["text"].(string); !ok || f1["reasoning"][0]["signature"] != "redacted-signature" {
		t.Errorf("reasoning final = %v", f1["reasoning"][0])
	}
	if f1["tool_call"][0]["file_path"] != "/work/hello.txt" || f1["tool_call"][1]["file_path"] != "/work/other.txt" {
		t.Errorf("tool_call finals = %v", f1["tool_call"])
	}
	if res, _ := f1["tool_result"][0]["text"].(string); !strings.Contains(res, "ORANGE-42") || f1["tool_result"][0]["is_error"] != false {
		t.Errorf("tool_result final = %v", f1["tool_result"][0])
	}
	d := deltas(e1)
	if d["text"] < 2 || d["tool_call"] < 2 {
		t.Errorf("turn 1 deltas by kind = %v, want streamed text, reasoning and tool arguments", d)
	}

	if tv := terminal(got[r2.ID]); tv.Verb != chatstream.VerbRunFinish {
		t.Errorf("turn 2 ends with %s", tv.Verb)
	}
	if f2 := finals(got[r2.ID]); len(f2["text"]) != 1 || f2["text"][0]["text"] != "done" {
		t.Errorf("turn 2 finals = %v", f2)
	}

	want := []string{string(UserFrame(turnsText1)), string(fx.stdin[1].Frame)}
	if in := h.in.all(); !reflect.DeepEqual(in, want) {
		t.Errorf("stdin = %q\nwant    %q", in, want)
	}
}

// Cancel mid-answer writes the interrupt request; the CLI's aborted result ends the run with run.abort, keeping the
// partial text. The next submitted turn runs on the same process.
func TestInterruptGolden(t *testing.T) {
	fx := loadFixture(t, "interrupt")
	h := newHarness(t, nil)
	h.replay(fx, nil, nil)
	h.settle()
	got := h.golden("interrupt")
	if len(h.runs) != 2 || h.runs[0].Observed || h.runs[1].Observed {
		t.Fatalf("runs = %+v", h.runs)
	}
	e1 := got[h.runs[0].ID]
	if tv := terminal(e1); tv.Verb != chatstream.VerbRunAbort || tv.Reason != "aborted_streaming" {
		t.Errorf("turn 1 ends with %s %s, want run.abort aborted_streaming", tv.Verb, tv.Reason)
	}
	if f := finals(e1); len(f["text"]) != 1 || !strings.Contains(f["text"][0]["text"].(string), "River") {
		t.Errorf("turn 1 finals = %v", f)
	}
	if tv := terminal(got[h.runs[1].ID]); tv.Verb != chatstream.VerbRunFinish {
		t.Errorf("turn 2 ends with %s", tv.Verb)
	}
	want := []string{
		string(UserFrame("Write a 600-word essay about rivers.")),
		`{"request":{"subtype":"interrupt"},"request_id":"chat_interrupt_1","type":"control_request"}`,
		string(UserFrame(doneText)),
	}
	if in := h.in.all(); !reflect.DeepEqual(in, want) {
		t.Errorf("stdin = %q\nwant    %q", in, want)
	}
}

// Another writer's frame written while a submitted turn runs: the CLI runs it next, as its own turn, which comes out
// as an observed run; the submitted run is not disturbed.
func TestQueuedGolden(t *testing.T) {
	fx := loadFixture(t, "queued")
	h := newHarness(t, nil)
	h.replay(fx, nil, nil)
	h.settle()
	got := h.golden("queued")
	if len(h.runs) != 2 {
		t.Fatalf("runs = %d, want 2", len(h.runs))
	}
	sub, obs := h.runs[0], h.runs[1]
	if sub.Observed || !obs.Observed || obs.Text != "Reply with only the word: second" {
		t.Fatalf("runs = %+v %+v", *sub, *obs)
	}
	if f := finals(got[sub.ID]); len(f["text"]) != 1 || !strings.Contains(f["text"][0]["text"].(string), "Rivers") {
		t.Errorf("submitted finals = %v", f)
	}
	if f := finals(got[obs.ID]); len(f["text"]) != 1 || f["text"][0]["text"] != "second" {
		t.Errorf("observed finals = %v", f)
	}
	for _, r := range h.runs {
		if tv := terminal(got[r.ID]); tv.Verb != chatstream.VerbRunFinish {
			t.Errorf("%s ends with %s", r.ID, tv.Verb)
		}
	}
}

// Submit while a turn runs, or while another writer's frame waits: ErrBusy, nothing written.
func TestSubmitBusy(t *testing.T) {
	fx := loadFixture(t, "queued")
	h := newHarness(t, nil)
	h.submit("first")
	if _, err := h.b.Submit(context.Background(), chatbackend.Turn{Text: "second"}); !errors.Is(err, chatbackend.ErrBusy) {
		t.Fatalf("submit with one waiting: %v, want ErrBusy", err)
	}
	h.line(fx.lines[0]) // system/init: the first is running
	if _, err := h.b.Submit(context.Background(), chatbackend.Turn{Text: "second"}); !errors.Is(err, chatbackend.ErrBusy) {
		t.Fatalf("submit during a turn: %v, want ErrBusy", err)
	}

	h2 := newHarness(t, nil)
	h2.other(UserFrame("a mail wake"))
	if _, err := h2.b.Submit(context.Background(), chatbackend.Turn{Text: "x"}); !errors.Is(err, chatbackend.ErrBusy) {
		t.Fatalf("submit with another writer's frame waiting: %v, want ErrBusy", err)
	}
	if n := len(h.in.all()); n != 1 {
		t.Errorf("stdin writes = %d, want 1", n)
	}
}

// A failed write leaves nothing waiting and ends the run.
func TestSubmitWriteError(t *testing.T) {
	h := newHarness(t, nil)
	h.in.err = errWrite
	if _, err := h.b.Submit(context.Background(), chatbackend.Turn{Text: "x"}); !errors.Is(err, errWrite) {
		t.Fatalf("submit: %v, want the write error", err)
	}
	h.in.err = nil
	h.submit("now it works")
}

// Cancel before the CLI starts the turn: the run aborts at once; when the turn starts it is interrupted, and it is
// not reported as an observed run.
func TestCancelBeforeStart(t *testing.T) {
	fx := loadFixture(t, "interrupt")
	h := newHarness(t, nil)
	r := h.submit("Write a 600-word essay about rivers.")
	r.Cancel()
	evs := events(t, r)
	checkStream(t, evs)
	if tv := terminal(evs); tv.Verb != chatstream.VerbRunAbort {
		t.Fatalf("terminal = %s, want run.abort", tv.Verb)
	}
	result := fx.lineIndex(t, `"type":"result"`)
	for _, l := range fx.lines[:result] {
		h.line(l)
	}
	h.settle()
	if len(h.runs) != 1 {
		t.Errorf("runs = %d, want only the submitted one", len(h.runs))
	}
	in := h.in.all()
	if len(in) != 2 || !strings.Contains(in[1], `"interrupt"`) {
		t.Errorf("stdin = %q, want the frame then the interrupt", in)
	}
	h.submit(doneText) // the canceled turn is over: not busy
}

// Cancel with no aborted result: CancelGrace aborts the run; the rest of that turn is dropped and the next turn is a
// run of its own.
func TestCancelWithoutAbortedResult(t *testing.T) {
	fx := loadFixture(t, "interrupt")
	h := newHarness(t, nil)
	cancelAt := fx.stdin[1].After
	result := fx.lineIndex(t, `"type":"result"`)
	r := h.submit(fx.stdin[0].text(t))
	for _, l := range fx.lines[:cancelAt] {
		h.line(l)
	}
	r.Cancel()
	r.Cancel()
	h.settle() // past CancelGrace, no aborted result yet
	evs := events(t, r)
	checkStream(t, evs)
	if tv := terminal(evs); tv.Verb != chatstream.VerbRunAbort {
		t.Errorf("terminal = %s, want run.abort", tv.Verb)
	}
	// The turn's late lines and its result reach no run; the following init starts the next.
	for _, l := range fx.lines[cancelAt:result] {
		h.line(l)
	}
	o := h.submit(doneText)
	for _, l := range fx.lines[result:] {
		h.line(l)
	}
	h.settle()
	if tv := terminal(events(t, o)); tv.Verb != chatstream.VerbRunFinish {
		t.Errorf("next turn ends with %s", tv.Verb)
	}
	if len(h.runs) != 2 {
		t.Errorf("runs = %d, want 2", len(h.runs))
	}
	if n := strings.Count(strings.Join(h.in.all(), "\n"), `"interrupt"`); n != 1 {
		t.Errorf("interrupts written = %d, want 1 (cancel twice sends one)", n)
	}
}

// A submitted turn the CLI does not start within ClaimTimeout ends with input_not_seen; if it starts later, it is an
// observed run with its text.
func TestSubmitNeverSeen(t *testing.T) {
	fx := loadFixture(t, "turns")
	h := newHarness(t, func(c *Config) { c.ClaimTimeout = time.Minute })
	r := h.submit(turnsText1)
	h.settle()
	evs := events(t, r)
	checkStream(t, evs)
	if tv := terminal(evs); tv.Verb != chatstream.VerbRunError || tv.Code != CodeInputNotSeen {
		t.Errorf("terminal = %s %s", tv.Verb, tv.Code)
	}
	h.line(fx.lines[0])
	if len(h.runs) != 2 || !h.runs[1].Observed || h.runs[1].Text != turnsText1 {
		t.Errorf("late turn = %+v", h.runs[len(h.runs)-1])
	}
}

// A turn nobody noted (a frame written around the Tap) is still reported, with no text.
func TestUnseenTurn(t *testing.T) {
	fx := loadFixture(t, "turns")
	h := newHarness(t, nil)
	start := 0
	for i, l := range fx.lines {
		if strings.Contains(string(l), `"subtype":"init"`) {
			start = i
		}
	}
	for _, l := range fx.lines[start:] {
		h.line(l)
	}
	h.settle()
	if len(h.runs) != 1 || h.runs[0].Source != SourceUnseen || h.runs[0].Text != "" {
		t.Fatalf("runs = %+v", h.runs)
	}
	if tv := terminal(events(t, h.runs[0].Run)); tv.Verb != chatstream.VerbRunFinish {
		t.Errorf("unseen turn ends with %s", tv.Verb)
	}
}

// End to end through Run: a Tap fed from another goroutine, an observed run on the Observed channel, and the session
// ending (Tap closed) mid-turn.
func TestRunEndToEnd(t *testing.T) {
	fx := loadFixture(t, "turns")
	tap := NewTap()
	in := &fakeStdin{}
	b := New(Config{Tap: tap, Stdin: in})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- b.Run(ctx) }()
	waitFor(t, func() bool { tap.mu.Lock(); defer tap.mu.Unlock(); return len(tap.subs) == 1 })

	r1, err := b.Submit(ctx, chatbackend.Turn{Text: turnsText1})
	if err != nil {
		t.Fatal(err)
	}
	second := fx.stdin[1]
	for i, l := range fx.lines {
		if i == second.After {
			if err := tap.Input(second.Frame, "send_turn", func() error { return in.Write(ctx, second.Frame) }); err != nil {
				t.Fatal(err)
			}
		}
		if i > second.After+3 {
			break // the session dies a few lines into the second turn
		}
		_, _ = tap.Write(append(append([]byte(nil), l...), '\n'))
	}
	var obs chatbackend.ObservedRun
	select {
	case obs = <-b.Observed():
	case <-ctx.Done():
		t.Fatal("no observed run")
	}
	tap.Close()
	collect := func(r chatbackend.Run) []chatstream.Event {
		var out []chatstream.Event
		for ev, err := range r.Events() {
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, ev)
		}
		return out
	}
	e1, e2 := collect(r1), collect(obs.Run)
	checkStream(t, e1)
	checkStream(t, e2)
	if terminal(e1).Verb != chatstream.VerbRunFinish {
		t.Errorf("turn 1 ends with %s", terminal(e1).Verb)
	}
	if tv := terminal(e2); tv.Verb != chatstream.VerbRunError || tv.Code != CodeSessionEnded {
		t.Errorf("turn 2 ends with %s %s, want run.error session_ended", tv.Verb, tv.Code)
	}
	if obs.Text != doneText {
		t.Errorf("observed text = %q", obs.Text)
	}
	if err := <-done; err != nil {
		t.Errorf("Run = %v, want nil when the session ended", err)
	}
	if _, err := b.Submit(context.Background(), chatbackend.Turn{Text: "x"}); !errors.Is(err, chatbackend.ErrClosed) {
		t.Errorf("submit after Run ended: %v", err)
	}
	if _, ok := <-b.Observed(); ok {
		t.Error("Observed not closed")
	}
}

func TestInfo(t *testing.T) {
	if got := New(Config{}).Info(); got != (chatbackend.Info{Deltas: true, Approvals: false}) {
		t.Errorf("Info = %+v", got)
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
