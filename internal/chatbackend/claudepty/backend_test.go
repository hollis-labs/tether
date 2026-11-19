package claudepty

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/libs/harness/core/adapters/runtimeevents"
	"github.com/hollis-labs/libs/ui/ui-go/chatstream"

	"github.com/hollis-labs/tether/internal/agentevents"
	"github.com/hollis-labs/tether/internal/chatbackend"
)

const (
	turn1Text = "Read hello.txt and other.txt (both at once, in parallel). Then work out carefully which code has more letters, thinking it through before you answer in one sentence."
	turn3Text = "Write a 600-word essay about rivers."
)

// The recorded session: turn 1 submitted (paste + Enter) with two parallel tool calls, thinking and text; turn 2 typed
// into the terminal; turn 3 submitted and canceled with Esc mid-answer.
func TestThreeTurnsGolden(t *testing.T) {
	fx := loadFixture(t, "three-turns")
	h := newHarness(t, nil)
	var run3 chatbackend.Run
	interrupt := fx.lineIndex(t, "[Request interrupted by user]")
	h.replay(fx, map[int]func(){
		0:             func() { h.submit(turn1Text) },
		50:            func() { run3 = h.submit(turn3Text) },
		interrupt - 1: func() { run3.Cancel() },
	}, nil)
	h.settle()
	got := h.golden("three-turns", "golden.json")

	if len(h.runs) != 3 {
		t.Fatalf("runs = %d, want 3", len(h.runs))
	}
	r1, r2, r3 := h.runs[0], h.runs[1], h.runs[2]
	if r1.Observed || !r2.Observed || r3.Observed {
		t.Fatalf("observed flags = %v %v %v, want false true false", r1.Observed, r2.Observed, r3.Observed)
	}
	if r2.Text != "Reply with only the word: done" || r2.Source != "typed" || r2.Origin != "human" || r2.TurnRef == "" {
		t.Errorf("observed run = %+v", *r2)
	}

	// Turn 1: two tool calls and their results, thinking, text, then run.finish. The final text line landed after the
	// Stop hook (after_line 40), and is still in the run.
	e1 := got[r1.ID]
	if tv := terminal(e1); tv.Verb != chatstream.VerbRunFinish || tv.Reason != string(chatstream.FinishStop) {
		t.Errorf("turn 1 ends with %s %s", tv.Verb, tv.Reason)
	}
	f1 := finals(e1)
	if len(f1["tool_call"]) != 2 || len(f1["tool_result"]) != 2 || len(f1["reasoning"]) != 1 || len(f1["text"]) != 1 {
		t.Fatalf("turn 1 parts: %d tool_call, %d tool_result, %d reasoning, %d text", len(f1["tool_call"]), len(f1["tool_result"]), len(f1["reasoning"]), len(f1["text"]))
	}
	if txt, _ := f1["text"][0]["text"].(string); !strings.Contains(txt, "ORANGE-42") {
		t.Errorf("turn 1 text final = %q", txt)
	}
	if res, _ := f1["tool_result"][0]["text"].(string); !strings.Contains(res, "The launch code is ORANGE-42.") || f1["tool_result"][0]["is_error"] != false {
		t.Errorf("tool_result final = %v", f1["tool_result"][0])
	}
	if f1["tool_call"][0]["file_path"] == nil {
		t.Errorf("tool_call final = %v", f1["tool_call"][0])
	}
	if f1["reasoning"][0]["signature"] != "redacted-signature" {
		t.Errorf("reasoning final = %v", f1["reasoning"][0])
	}

	if tv := terminal(got[r2.ID]); tv.Verb != chatstream.VerbRunFinish {
		t.Errorf("turn 2 ends with %s", tv.Verb)
	}
	if f2 := finals(got[r2.ID]); len(f2["text"]) != 1 || f2["text"][0]["text"] != "done" {
		t.Errorf("turn 2 finals = %v", f2)
	}

	// Turn 3: canceled; the partial text is kept and the run aborts.
	e3 := got[r3.ID]
	if tv := terminal(e3); tv.Verb != chatstream.VerbRunAbort {
		t.Errorf("turn 3 ends with %s, want run.abort", tv.Verb)
	}
	if f3 := finals(e3); len(f3["text"]) != 1 || !strings.Contains(f3["text"][0]["text"].(string), "River") {
		t.Errorf("turn 3 finals = %v", f3)
	}

	wantInput := []string{"\x1b[200~" + turn1Text + "\x1b[201~", "\r", "\x1b[200~" + turn3Text + "\x1b[201~", "\r", "\x1b"}
	if in := h.in.all(); !reflect.DeepEqual(in, wantInput) {
		t.Errorf("input = %q\nwant  %q", in, wantInput)
	}
}

// Input pasted while a mail wake ran: the TUI folded it into the running turn (a queued_command attachment), so the
// submitted run ends with input_merged and the observed run reports the input.
func TestQueuedMergeGolden(t *testing.T) {
	fx := loadFixture(t, "queued-merge")
	h := newHarness(t, nil)
	h.replay(fx, map[int]func(){
		0: func() { h.submit("Read hello.txt and tell me the launch code in one short sentence.") },
	}, nil)
	h.settle()
	got := h.golden("queued-merge", "golden.json")
	if len(h.runs) != 2 {
		t.Fatalf("runs = %d, want 2", len(h.runs))
	}
	sub, obs := h.runs[0], h.runs[1]
	if tv := terminal(got[sub.ID]); tv.Verb != chatstream.VerbRunError || tv.Code != CodeInputMerged {
		t.Errorf("submitted run ends with %s %s", tv.Verb, tv.Code)
	}
	if !obs.Observed || obs.Source != "system" || !strings.Contains(obs.Text, "tether-mail") {
		t.Errorf("observed run = %+v", *obs)
	}
	var queued bool
	for _, ev := range got[obs.ID] {
		if ev.Verb == chatstream.VerbActivity && ev.Kind == ActivityQueuedInput && strings.Contains(string(ev.Value), "launch code") {
			queued = true
		}
	}
	if !queued {
		t.Error("observed run has no queued-input activity")
	}
	if tv := terminal(got[obs.ID]); tv.Verb != chatstream.VerbRunFinish {
		t.Errorf("observed run ends with %s", tv.Verb)
	}
}

// When the transcript's end markers never arrive, the Stop hook's grace timer ends the run, with everything the
// transcript did write, as run.finish reason other.
func TestStopWithoutTranscriptMarker(t *testing.T) {
	fx := loadFixture(t, "three-turns")
	h := newHarness(t, nil)
	skip := map[int]bool{}
	for i, l := range fx.lines {
		if strings.Contains(string(l), `"stop_hook_summary"`) || strings.Contains(string(l), `"turn_duration"`) {
			skip[i+1] = true
		}
	}
	stop := 0
	for _, hk := range fx.hooks {
		if strings.Contains(string(hk.Payload), `"Stop"`) {
			stop = hk.After
			break
		}
	}
	for i := fx.lineIndex(t, "Reply with only the word: done"); i <= len(fx.lines); i++ {
		skip[i] = true // turn 1 only: nothing after it supersedes it
	}
	h.replay(fx, map[int]func(){0: func() { h.submit(turn1Text) }}, skip)
	h.settle()
	if len(h.runs) < 1 {
		t.Fatal("no runs")
	}
	evs := events(t, h.runs[0].Run)
	checkCommitted(t, evs)
	tv := terminal(evs)
	if tv.Verb != chatstream.VerbRunFinish || tv.Reason != string(chatstream.FinishOther) || tv.RawReason != "transcript_forced" {
		t.Errorf("terminal = %s %s %s, want run.finish other transcript_forced", tv.Verb, tv.Reason, tv.RawReason)
	}
	if f := finals(evs); len(f["text"]) != 1 {
		t.Errorf("text parts = %d (stop hook after line %d), want the final text that landed after Stop", len(f["text"]), stop)
	}
}

// Cancel with no interrupt line in the transcript: CancelGrace aborts the run anyway.
func TestCancelWithoutInterruptLine(t *testing.T) {
	fx := loadFixture(t, "three-turns")
	h := newHarness(t, nil)
	interrupt := fx.lineIndex(t, "[Request interrupted by user]")
	var run3 chatbackend.Run
	h.replay(fx, map[int]func(){
		50:            func() { run3 = h.submit(turn3Text) },
		interrupt - 1: func() { run3.Cancel(); run3.Cancel() },
	}, map[int]bool{interrupt: true})
	h.settle()
	evs := events(t, run3)
	checkCommitted(t, evs)
	if tv := terminal(evs); tv.Verb != chatstream.VerbRunAbort {
		t.Errorf("terminal = %s, want run.abort", tv.Verb)
	}
	esc := 0
	for _, w := range h.in.all() {
		if w == "\x1b" {
			esc++
		}
	}
	if esc != 1 {
		t.Errorf("Esc pressed %d times, want 1", esc)
	}
}

// Cancel before the submitted text shows up as a turn: no Esc (the TUI is idle; two Escs open its rewind menu), and the
// run aborts after CancelGrace.
func TestCancelPendingSubmit(t *testing.T) {
	h := newHarness(t, nil)
	r := h.submit("hello")
	r.Cancel()
	h.settle()
	evs := events(t, r)
	checkCommitted(t, evs)
	if tv := terminal(evs); tv.Verb != chatstream.VerbRunAbort {
		t.Errorf("terminal = %s, want run.abort", tv.Verb)
	}
	for _, w := range h.in.all() {
		if w == "\x1b" {
			t.Error("Esc sent for a turn that never started")
		}
	}
}

func TestSubmitNeverSeen(t *testing.T) {
	h := newHarness(t, nil)
	r := h.submit("hello")
	h.settle()
	evs := events(t, r)
	checkCommitted(t, evs)
	if tv := terminal(evs); tv.Verb != chatstream.VerbRunError || tv.Code != CodeInputNotSeen {
		t.Errorf("terminal = %s %s", tv.Verb, tv.Code)
	}
}

func TestSubmitBusy(t *testing.T) {
	fx := loadFixture(t, "three-turns")
	h := newHarness(t, nil)
	h.hook(fx.hooks[1].Payload) // UserPromptSubmit: a turn is open before its line lands
	if _, err := h.b.Submit(context.Background(), chatbackend.Turn{Text: "x"}); !errors.Is(err, chatbackend.ErrBusy) {
		t.Fatalf("submit during a hook-open turn: %v, want ErrBusy", err)
	}
	h2 := newHarness(t, nil)
	h2.submit("first")
	if _, err := h2.b.Submit(context.Background(), chatbackend.Turn{Text: "second"}); !errors.Is(err, chatbackend.ErrBusy) {
		t.Fatalf("submit with one pending: %v, want ErrBusy", err)
	}
}

func TestSubmitInputError(t *testing.T) {
	h := newHarness(t, nil)
	h.in.err = errors.New("pty closed")
	if _, err := h.b.Submit(context.Background(), chatbackend.Turn{Text: "x"}); err == nil {
		t.Fatal("want an error")
	}
	h.in.err = nil
	h.submit("now it works") // the failed submit left nothing pending
}

func TestSessionEndDuringTurn(t *testing.T) {
	fx := loadFixture(t, "three-turns")
	h := newHarness(t, nil)
	interrupt := fx.lineIndex(t, "[Request interrupted by user]")
	var run3 chatbackend.Run
	h.replay(fx, map[int]func(){50: func() { run3 = h.submit(turn3Text) }}, map[int]bool{interrupt: true})
	evs := events(t, run3) // the fixture's SessionEnd hook ended it
	checkCommitted(t, evs)
	if tv := terminal(evs); tv.Verb != chatstream.VerbRunError || tv.Code != "session_ended" {
		t.Errorf("terminal = %s %s", tv.Verb, tv.Code)
	}
}

func TestPasteStripsControls(t *testing.T) {
	got := string(pasteBytes("a\x1b[201~b\r\nc\x07"))
	if got != "\x1b[200~a[201~b\nc\x1b[201~" {
		t.Errorf("paste = %q", got)
	}
	if !sameInput("a  b\r\nc", "a b\nc") {
		t.Error("whitespace differences should match")
	}
}

// End to end on real files: the hook spool written by agentevents.Spool (as `tether hook claude` does) and the
// transcript written line by line, followed by Backend.Run.
func TestLiveFilesEndToEnd(t *testing.T) {
	fx := loadFixture(t, "three-turns")
	dir := t.TempDir()
	spoolDir := filepath.Join(dir, "events")
	projects := filepath.Join(dir, "projects")
	const claudeSID = "00000000-0000-4000-8000-000000000001"
	tpath := filepath.Join(projects, "-work", claudeSID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(tpath), 0o700); err != nil {
		t.Fatal(err)
	}
	in := &fakeInput{}
	var b *Backend
	b = New(Config{
		SessionID: "sess-test", Input: in, EnterDelay: time.Millisecond, FlushGrace: 2 * time.Second,
		Hooks:      &spoolHooks{path: filepath.Join(spoolDir, "sess-test.jsonl"), poll: 10 * time.Millisecond},
		Transcript: &sessionTranscript{dir: projects, sessionID: func() string { return b.ClaudeSessionID() }, poll: 10 * time.Millisecond},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- b.Run(ctx) }()

	spool := &agentevents.Spool{Dir: spoolDir}
	writeHook := func(payload []byte) {
		obs, err := agentevents.ClaudeHook(payload)
		if err != nil {
			t.Fatal(err)
		}
		rec := &agentevents.Recorder{App: "tether", SessionID: "sess-test", Sink: spool,
			Process: runtimeevents.Process{Provider: "claude", Runtime: "hooks", ProviderSessionID: agentevents.ClaudeSessionID(payload)}}
		for _, o := range obs {
			if _, err := rec.Record(ctx, o); err != nil {
				t.Fatal(err)
			}
		}
	}
	tf, err := os.OpenFile(tpath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // test file
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tf.Close() }()
	hooksAt := map[int][][]byte{}
	for _, hk := range fx.hooks {
		hooksAt[hk.After] = append(hooksAt[hk.After], hk.Payload)
	}
	// Turn 1 and 2 only: write up to turn 2's last line.
	last := fx.lineIndex(t, "Reply with only the word: done") + 4
	writeHook(hooksAt[0][0]) // SessionStart
	waitFor(t, func() bool { return b.ClaudeSessionID() == claudeSID })
	r1, err := b.Submit(ctx, chatbackend.Turn{Text: turn1Text})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range hooksAt[0][1:] { // UserPromptSubmit, which the paste caused
		writeHook(p)
	}
	for i := 1; i <= last; i++ {
		if _, err := tf.Write(append(fx.lines[i-1], '\n')); err != nil {
			t.Fatal(err)
		}
		for _, p := range hooksAt[i] {
			writeHook(p)
		}
		time.Sleep(2 * time.Millisecond)
	}
	var obs chatbackend.ObservedRun
	select {
	case obs = <-b.Observed():
	case <-ctx.Done():
		t.Fatal("no observed run")
	}
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
	checkCommitted(t, e1)
	checkCommitted(t, e2)
	if terminal(e1).Verb != chatstream.VerbRunFinish || terminal(e2).Verb != chatstream.VerbRunFinish {
		t.Errorf("terminals %s %s", terminal(e1).Verb, terminal(e2).Verb)
	}
	if obs.Text != "Reply with only the word: done" {
		t.Errorf("observed text = %q", obs.Text)
	}
	cancel()
	<-done
	if _, err := b.Submit(context.Background(), chatbackend.Turn{Text: "x"}); !errors.Is(err, chatbackend.ErrClosed) {
		t.Errorf("submit after Run ended: %v", err)
	}
	if _, ok := <-b.Observed(); ok {
		t.Error("Observed not closed")
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
