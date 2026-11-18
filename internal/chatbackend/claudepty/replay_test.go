package claudepty

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/libs/harness/core/adapters/runtimeevents"
	"github.com/hollis-labs/libs/ui/ui-go/chatstream"
	"github.com/hollis-labs/libs/ui/ui-go/chatstream/conformance"

	"github.com/hollis-labs/tether/internal/agentevents"
	"github.com/hollis-labs/tether/internal/chatbackend"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// The fixtures under testdata were recorded from Claude Code 2.1.327 driving the real TUI in a PTY (submit by bracketed
// paste + Enter, typing, Esc), with every hook logging its payload and how many transcript lines existed when it fired.
// They are redacted (paths, signatures, tool output in queued-merge) and trimmed (api-request blobs, attachment
// bodies); the line order and the hook positions are the recorded ones. A hooks.jsonl line is
// {"after_line": n, "payload": <hook payload>}: the hook fired when the transcript had n lines.

type hookAt struct {
	After   int             `json:"after_line"`
	Payload json.RawMessage `json:"payload"`
}

type fixture struct {
	lines [][]byte
	hooks []hookAt
}

func loadFixture(t *testing.T, name string) fixture {
	t.Helper()
	var fx fixture
	f, err := os.Open(filepath.Join("testdata", name, "transcript.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		fx.lines = append(fx.lines, append([]byte(nil), sc.Bytes()...))
	}
	raw, err := os.ReadFile(filepath.Join("testdata", name, "hooks.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var h hookAt
		if err := json.Unmarshal([]byte(l), &h); err != nil {
			t.Fatal(err)
		}
		fx.hooks = append(fx.hooks, h)
	}
	return fx
}

// lineIndex is the 1-based index of the first line containing s.
func (fx fixture) lineIndex(t *testing.T, s string) int {
	t.Helper()
	for i, l := range fx.lines {
		if strings.Contains(string(l), s) {
			return i + 1
		}
	}
	t.Fatalf("no line contains %q", s)
	return 0
}

// memSink keeps what a Recorder appends, as the spool would.
type memSink struct{ evs []runtimeevents.Event }

func (m *memSink) Append(_ context.Context, ev runtimeevents.Event) (runtimeevents.Event, error) {
	ev.Sequence = uint64(len(m.evs) + 1) //nolint:gosec // G115: test counter
	m.evs = append(m.evs, ev)
	return ev, nil
}

// hookSignals runs a payload through the production mapping (agentevents.ClaudeHook, Recorder) and back out through
// what SpoolHooks does with a spooled event.
func hookSignals(t *testing.T, payload []byte, now time.Time) []HookSignal {
	t.Helper()
	obs, err := agentevents.ClaudeHook(payload)
	if err != nil {
		t.Fatal(err)
	}
	sink := &memSink{}
	rec := &agentevents.Recorder{App: "tether", SessionID: "sess-test", Sink: sink, Now: func() time.Time { return now },
		Process: runtimeevents.Process{Provider: "claude", Runtime: "hooks", ProviderSessionID: agentevents.ClaudeSessionID(payload)}}
	var out []HookSignal
	for _, o := range obs {
		ev, err := rec.Record(context.Background(), o)
		if err != nil {
			t.Fatal(err)
		}
		if s, ok := hookSignal(ev); ok {
			out = append(out, s)
		}
	}
	return out
}

// fakeInput records what the backend typed.
type fakeInput struct {
	mu     sync.Mutex
	writes []string
	err    error
}

func (f *fakeInput) SendInput(id string, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id != "sess-test" {
		return fmt.Errorf("wrong session %q", id)
	}
	if f.err != nil {
		return f.err
	}
	f.writes = append(f.writes, string(data))
	return nil
}

func (f *fakeInput) all() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.writes...)
}

// harness replays a fixture into a Backend on a fake clock.
type harness struct {
	t     *testing.T
	b     *Backend
	in    *fakeInput
	now   time.Time
	runs  []*recordedRun
	runID int
}

type recordedRun struct {
	Run      chatbackend.Run `json:"-"`
	ID       string          `json:"run_id"`
	Observed bool            `json:"observed"`
	Text     string          `json:"text,omitempty"`
	Source   string          `json:"source,omitempty"`
	Origin   string          `json:"origin,omitempty"`
	TurnRef  string          `json:"turn_ref,omitempty"`
}

func newHarness(t *testing.T, mod func(*Config)) *harness {
	h := &harness{t: t, in: &fakeInput{}, now: time.Date(2026, 11, 18, 16, 38, 0, 0, time.UTC)}
	cfg := Config{
		SessionID: "sess-test", Input: h.in, Transcript: nopSource{}, EnterDelay: time.Nanosecond,
		Now:      func() time.Time { return h.now },
		NewRunID: func() string { h.runID++; return fmt.Sprintf("run-%d", h.runID) },
	}
	if mod != nil {
		mod(&cfg)
	}
	h.b = New(cfg)
	return h
}

type nopSource struct{}

func (nopSource) Run(ctx context.Context, _ func([]byte)) error { <-ctx.Done(); return ctx.Err() }

func (h *harness) step() { h.now = h.now.Add(10 * time.Millisecond); h.b.tick(h.now) }

func (h *harness) hook(payload []byte) {
	h.step()
	for _, s := range hookSignals(h.t, payload, h.now) {
		h.b.onHook(s)
	}
}

func (h *harness) line(raw []byte) { h.step(); h.b.onLine(raw); h.collectObserved() }

func (h *harness) collectObserved() {
	h.b.mu.Lock()
	q := h.b.observedQ
	h.b.observedQ = nil
	h.b.mu.Unlock()
	for _, o := range q {
		h.runs = append(h.runs, &recordedRun{Run: o.Run, ID: o.ID(), Observed: true, Text: o.Text, Source: o.Source, Origin: o.Origin, TurnRef: o.TurnRef})
	}
}

func (h *harness) submit(text string) chatbackend.Run {
	h.t.Helper()
	r, err := h.b.Submit(context.Background(), chatbackend.Turn{Text: text})
	if err != nil {
		h.t.Fatalf("submit: %v", err)
	}
	h.runs = append(h.runs, &recordedRun{Run: r, ID: r.ID(), Text: text})
	return r
}

// replay plays the fixture: at[n] runs after line n was written and before the hooks that fired at n (at[0]: before
// anything). Lines in skip are left out (a transcript that never got them).
func (h *harness) replay(fx fixture, at map[int]func(), skip map[int]bool) {
	hooksAt := map[int][]json.RawMessage{}
	for _, hk := range fx.hooks {
		hooksAt[hk.After] = append(hooksAt[hk.After], hk.Payload)
	}
	run := func(n int) {
		if f := at[n]; f != nil {
			f()
		}
		for _, p := range hooksAt[n] {
			h.hook(p)
		}
	}
	run(0)
	for i, l := range fx.lines {
		if !skip[i+1] {
			h.line(l)
		}
		run(i + 1)
	}
}

// settle advances the clock past every grace timer.
func (h *harness) settle() {
	for range 3 {
		h.now = h.now.Add(time.Minute)
		h.b.tick(h.now)
	}
	h.collectObserved()
}

// events drains a run that must have ended.
func events(t *testing.T, r chatbackend.Run) []chatstream.Event {
	t.Helper()
	s, ok := r.(*chatbackend.Stream)
	if !ok || !s.Done() {
		t.Fatalf("run %s has not ended", r.ID())
	}
	var out []chatstream.Event
	for ev, err := range r.Events() {
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, ev)
	}
	return out
}

// checkCommitted asserts the rules a committed-only client relies on: a well-formed stream, no part.delta, a final on
// every part.end, and each message opened once.
func checkCommitted(t *testing.T, evs []chatstream.Event) {
	t.Helper()
	conformance.Check(t, evs)
	msgs := map[string]int{}
	for _, ev := range evs {
		switch ev.Verb {
		case chatstream.VerbPartDelta:
			t.Errorf("part.delta %s on a backend without deltas", ev.PartID)
		case chatstream.VerbPartEnd:
			if len(ev.Final) == 0 {
				t.Errorf("part.end %s has no final", ev.PartID)
			}
		case chatstream.VerbMessageStart:
			msgs[ev.MessageID]++
			if msgs[ev.MessageID] > 1 {
				t.Errorf("message %s opened twice", ev.MessageID)
			}
		default:
		}
	}
}

type goldenRun struct {
	recordedRun
	Events []chatstream.Event `json:"events"`
}

// golden compares every run's events (times cleared) with testdata/<name>/<file>.
func (h *harness) golden(name, file string) map[string][]chatstream.Event {
	h.t.Helper()
	byID := map[string][]chatstream.Event{}
	var out []goldenRun
	for _, r := range h.runs {
		evs := events(h.t, r.Run)
		checkCommitted(h.t, evs)
		byID[r.ID] = evs
		norm := make([]chatstream.Event, len(evs))
		for i, ev := range evs {
			ev.Time = time.Time{}
			norm[i] = ev
		}
		out = append(out, goldenRun{recordedRun: *r, Events: norm})
	}
	got, err := json.MarshalIndent(out, "", " ")
	if err != nil {
		h.t.Fatal(err)
	}
	path := filepath.Join("testdata", name, file)
	if *update {
		if err := os.WriteFile(path, append(got, '\n'), 0o600); err != nil {
			h.t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		h.t.Fatalf("%v (run with -update to create it)", err)
	}
	if strings.TrimSpace(string(want)) != strings.TrimSpace(string(got)) {
		h.t.Errorf("%s differs from the events produced (run go test -run %s -update and review the diff)", path, h.t.Name())
	}
	return byID
}

func terminal(evs []chatstream.Event) chatstream.Event { return evs[len(evs)-1] }

// finals maps part kind to the finals of that kind, in order.
func finals(evs []chatstream.Event) map[string][]map[string]any {
	kinds := map[string]string{}
	out := map[string][]map[string]any{}
	for _, ev := range evs {
		switch ev.Verb {
		case chatstream.VerbPartStart:
			kinds[ev.PartID] = ev.Kind
		case chatstream.VerbPartEnd:
			var m map[string]any
			_ = json.Unmarshal(ev.Final, &m)
			out[kinds[ev.PartID]] = append(out[kinds[ev.PartID]], m)
		default:
		}
	}
	return out
}
