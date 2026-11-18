package claudestdio

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/libs/ui/ui-go/chatstream"
	"github.com/hollis-labs/libs/ui/ui-go/chatstream/conformance"

	"github.com/hollis-labs/tether/internal/chatbackend"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// The fixtures under testdata were recorded from Claude Code 2.1.327 run as
// `claude -p --input-format stream-json --output-format stream-json --verbose --include-partial-messages` (model sonnet,
// thinking on, the Read tool only), driven by a script that wrote stream-json frames to stdin and logged how many
// stdout lines existed when it wrote each one. stdout.jsonl is the recorded stdout, redacted (the init line trimmed to
// its stable fields, paths, session ids, thinking signatures, usage detail) and trimmed (runs of text deltas merged up
// to eight at a time, tool-argument deltas merged per segment); the line order and the stdin positions are the
// recorded ones. A stdin.jsonl line is {"after_line": n, "note": ..., "frame": <frame>}: the frame was written when
// stdout had n lines. note is "submit" (a chat Submit), "other_writer" (another writer, as SendTurn) or "cancel" (the
// interrupt request a Cancel writes).
//
//   - turns: a submitted turn with two parallel Read calls, thinking and text; then a turn written by another writer.
//   - interrupt: a submitted turn interrupted mid-answer (the CLI ends it with an aborted_streaming result), then a
//     submitted turn on the same process.
//   - queued: another writer's frame written while a submitted turn runs; the CLI runs it after, as its own turn.

type stdinAt struct {
	After int             `json:"after_line"`
	Note  string          `json:"note"`
	Frame json.RawMessage `json:"frame"`
}

type fixture struct {
	lines [][]byte
	stdin []stdinAt
}

func loadFixture(t *testing.T, name string) fixture {
	t.Helper()
	var fx fixture
	f, err := os.Open(filepath.Join("testdata", name, "stdout.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		fx.lines = append(fx.lines, append([]byte(nil), sc.Bytes()...))
	}
	raw, err := os.ReadFile(filepath.Join("testdata", name, "stdin.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var s stdinAt
		if err := json.Unmarshal([]byte(l), &s); err != nil {
			t.Fatal(err)
		}
		fx.stdin = append(fx.stdin, s)
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

func (s stdinAt) text(t *testing.T) string {
	t.Helper()
	text, ok := UserFrameText(s.Frame)
	if !ok {
		t.Fatalf("stdin frame is not a user frame: %s", s.Frame)
	}
	return text
}

// fakeStdin records what reached stdin.
type fakeStdin struct {
	mu     sync.Mutex
	writes []string
	err    error
}

func (f *fakeStdin) Write(_ context.Context, frame []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.writes = append(f.writes, string(frame))
	return nil
}

func (f *fakeStdin) all() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.writes...)
}

// harness replays a fixture into a Backend subscribed to a real Tap, on a fake clock.
type harness struct {
	t     *testing.T
	b     *Backend
	tap   *Tap
	in    *fakeStdin
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
	h := &harness{t: t, in: &fakeStdin{}, tap: NewTap(), now: time.Date(2026, 11, 18, 18, 0, 0, 0, time.UTC)}
	cfg := Config{
		Tap: h.tap, Stdin: h.in,
		Now:      func() time.Time { return h.now },
		NewRunID: func() string { h.runID++; return fmt.Sprintf("run-%d", h.runID) },
	}
	if mod != nil {
		mod(&cfg)
	}
	h.b = New(cfg)
	t.Cleanup(h.tap.Subscribe(h.b.subscriber()))
	return h
}

func (h *harness) step() { h.now = h.now.Add(10 * time.Millisecond); h.b.tick(h.now) }

// line writes one stdout line through the Tap, split in two writes to exercise reassembly.
func (h *harness) line(raw []byte) {
	h.step()
	data := append(append([]byte(nil), raw...), '\n')
	mid := len(data) / 2
	_, _ = h.tap.Write(data[:mid])
	_, _ = h.tap.Write(data[mid:])
	h.b.bg.Wait()
	h.collectObserved()
}

// other writes a frame to stdin the way internal/app's SendTurn does: through the Tap, labeled.
func (h *harness) other(frame []byte) {
	h.t.Helper()
	if err := h.tap.Input(frame, "send_turn", func() error { return h.in.Write(context.Background(), frame) }); err != nil {
		h.t.Fatal(err)
	}
}

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

// replay plays the fixture: stdin entries as recorded (submit through the backend, other writers through the Tap,
// cancel on the latest submitted run), then at[n] after line n (at[0]: before anything). Lines in skip are left out.
func (h *harness) replay(fx fixture, at map[int]func(), skip map[int]bool) {
	stdinAt := map[int][]stdinAt{}
	for _, s := range fx.stdin {
		stdinAt[s.After] = append(stdinAt[s.After], s)
	}
	var last chatbackend.Run
	run := func(n int) {
		for _, s := range stdinAt[n] {
			switch s.Note {
			case "submit":
				last = h.submit(s.text(h.t))
			case "other_writer":
				h.other(s.Frame)
			case "cancel":
				last.Cancel()
			default:
				h.t.Fatalf("unknown stdin note %q", s.Note)
			}
		}
		if f := at[n]; f != nil {
			f()
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

// settle advances the clock past every timer.
func (h *harness) settle() {
	for range 3 {
		h.now = h.now.Add(time.Hour)
		h.b.tick(h.now)
	}
	h.b.bg.Wait()
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

// checkStream asserts what a client relies on: a well-formed stream, a final on every part.end, and each message
// opened once.
func checkStream(t *testing.T, evs []chatstream.Event) {
	t.Helper()
	conformance.Check(t, evs)
	msgs := map[string]int{}
	for _, ev := range evs {
		switch ev.Verb {
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

// deltas counts part.delta events by the kind of their part.
func deltas(evs []chatstream.Event) map[string]int {
	kinds := map[string]string{}
	out := map[string]int{}
	for _, ev := range evs {
		switch ev.Verb {
		case chatstream.VerbPartStart:
			kinds[ev.PartID] = ev.Kind
		case chatstream.VerbPartDelta:
			out[kinds[ev.PartID]]++
		default:
		}
	}
	return out
}

type goldenRun struct {
	recordedRun
	Events []chatstream.Event `json:"events"`
}

// golden compares every run's events (times cleared) with testdata/<name>/golden.json.
func (h *harness) golden(name string) map[string][]chatstream.Event {
	h.t.Helper()
	byID := map[string][]chatstream.Event{}
	var out []goldenRun
	for _, r := range h.runs {
		evs := events(h.t, r.Run)
		checkStream(h.t, evs)
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
	path := filepath.Join("testdata", name, "golden.json")
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

var errWrite = errors.New("stdin closed")
