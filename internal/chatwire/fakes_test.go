package chatwire

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hollis-labs/tether/internal/chatbackend/claudepty"
	"github.com/hollis-labs/tether/internal/chatbackend/claudestdio"
)

// The fake sessions below drive the real Claude backends (claudepty, claudestdio) with the lines the recorded fixtures
// of CW-20261118-0010 hold, played back when the backend types or writes a turn. They answer any text with the
// recorded "done" turn, and longText with the recorded essay turn, which runs until it is interrupted.

const longText = "Write a 600-word essay about rivers."

func readLines(t testing.TB, path string) [][]byte {
	t.Helper()
	f, err := os.Open(path) //nolint:gosec // G304: test fixture path
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var out [][]byte
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		out = append(out, append([]byte(nil), sc.Bytes()...))
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func indexOf(t testing.TB, lines [][]byte, s string) int {
	t.Helper()
	for i, l := range lines {
		if bytes.Contains(l, []byte(s)) {
			return i
		}
	}
	t.Fatalf("no fixture line contains %q", s)
	return -1
}

// player writes lines in order from one goroutine.
type player struct {
	q chan func()
}

func newPlayer(t testing.TB) *player {
	p := &player{q: make(chan func(), 64)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for f := range p.q {
			f()
		}
	}()
	t.Cleanup(func() { close(p.q); <-done })
	return p
}

// ---- Claude streaming-stdio ----

// fakeStdio is the CLI behind a claudestdio.Backend: its stdin, writing its stdout to the tap.
type fakeStdio struct {
	tap          *claudestdio.Tap
	play         *player
	done, essay  [][]byte // the "done" turn; the essay turn up to the interrupt
	essayAborted [][]byte // the essay turn from the interrupt to its result
	writes       atomic.Int64
}

func newFakeStdio(t testing.TB) *fakeStdio {
	lines := readLines(t, filepath.Join("..", "chatbackend", "claudestdio", "testdata", "interrupt", "stdout.jsonl"))
	cut := indexOf(t, lines, `"control_response"`)
	second := cut + indexOf(t, lines[cut:], `"subtype":"init"`)
	return &fakeStdio{tap: claudestdio.NewTap(), play: newPlayer(t), essay: lines[:cut], essayAborted: lines[cut:second], done: lines[second:]}
}

func (f *fakeStdio) out(lines [][]byte) {
	f.play.q <- func() {
		for _, l := range lines {
			_, _ = f.tap.Write(append(append([]byte(nil), l...), '\n'))
		}
	}
}

// Write implements claudestdio.Stdin.
func (f *fakeStdio) Write(_ context.Context, frame []byte) error {
	f.writes.Add(1)
	if text, ok := claudestdio.UserFrameText(frame); ok {
		if text == longText {
			f.out(f.essay)
		} else {
			f.out(f.done)
		}
		return nil
	}
	if bytes.Contains(frame, []byte(`"interrupt"`)) {
		f.out(f.essayAborted)
	}
	return nil
}

func (f *fakeStdio) backend() Backend {
	return claudestdio.New(claudestdio.Config{Tap: f.tap, Stdin: f})
}

// ---- Claude TUI in a PTY ----

// fakePTY is the Claude Code TUI behind a claudepty.Backend: its terminal input, and its transcript.
type fakePTY struct {
	mu          sync.Mutex
	session     string
	lines       chan []byte
	pasted      string
	turns       int
	done, essay [][]byte // template turns from the recorded transcript
	interrupted []byte   // the essay turn's interrupt line
}

func newFakePTY(t testing.TB, session string) *fakePTY {
	lines := readLines(t, filepath.Join("..", "chatbackend", "claudepty", "testdata", "three-turns", "transcript.jsonl"))
	d := indexOf(t, lines, `"content":"Reply with only the word: done"`)
	e := indexOf(t, lines, `"content":"Write a 600-word essay about rivers."`)
	return &fakePTY{
		session: session, lines: make(chan []byte, 256),
		done: lines[d : d+5], essay: lines[e : e+4], interrupted: lines[e+4],
	}
}

// Run implements claudepty.LineSource.
func (f *fakePTY) Run(ctx context.Context, emit func([]byte)) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case l := <-f.lines:
			emit(l)
		}
	}
}

// turn rewrites a template turn for text with a fresh prompt id.
func (f *fakePTY) turn(tpl [][]byte, from, text string) {
	f.turns++
	q, _ := json.Marshal(text)
	id := fmt.Sprintf("00000000-0000-4000-8000-%012d", f.turns)
	for _, l := range tpl {
		s := strings.ReplaceAll(string(l), `"content":`+mustQuote(from), `"content":`+string(q))
		s = promptIDs.ReplaceAllString(s, `"promptId":"`+id+`"`)
		f.lines <- []byte(s)
	}
}

var promptIDs = regexp.MustCompile(`"promptId":"[^"]*"`)

func mustQuote(s string) string { b, _ := json.Marshal(s); return string(b) }

// SendInput implements claudepty.Input: a bracketed paste, Enter, or Esc.
func (f *fakePTY) SendInput(id string, data []byte) error {
	if id != f.session {
		return fmt.Errorf("input for session %q, want %q", id, f.session)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	s := string(data)
	switch {
	case strings.HasPrefix(s, "\x1b[200~"):
		f.pasted = strings.TrimSuffix(strings.TrimPrefix(s, "\x1b[200~"), "\x1b[201~")
	case s == "\r":
		if f.pasted == longText {
			f.turn(f.essay, longText, longText)
		} else {
			f.turn(f.done, "Reply with only the word: done", f.pasted)
		}
		f.pasted = ""
	case s == "\x1b":
		q := promptIDs.ReplaceAllString(string(f.interrupted), fmt.Sprintf(`"promptId":"00000000-0000-4000-8000-%012d"`, f.turns))
		f.lines <- []byte(q)
	}
	return nil
}

func (f *fakePTY) backend() Backend {
	return claudepty.New(claudepty.Config{SessionID: f.session, Input: f, Transcript: f})
}

// ---- Sessions ----

// fakeSessions serves sessions from fakes, by session ID.
type fakeSessions struct {
	t        testing.TB
	mu       sync.Mutex
	runtimes map[string]Runtime
	built    map[string]int
}

func newFakeSessions(t testing.TB) *fakeSessions {
	return &fakeSessions{t: t, runtimes: map[string]Runtime{}, built: map[string]int{}}
}

func (s *fakeSessions) set(session string, rt Runtime) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runtimes[session] = rt
}

func (s *fakeSessions) ChatRuntime(session string) (Runtime, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rt, ok := s.runtimes[session]
	if !ok {
		return "", &UnsupportedRuntimeError{SessionID: session, Reason: "unknown test session"}
	}
	return rt, nil
}

func (s *fakeSessions) ChatBackend(session string, rt Runtime) (Backend, error) {
	s.mu.Lock()
	s.built[session]++
	s.mu.Unlock()
	switch rt {
	case RuntimeClaudePTY:
		return newFakePTY(s.t, session).backend(), nil
	case RuntimeClaudeStdio:
		return newFakeStdio(s.t).backend(), nil
	case RuntimeCodexAppServer:
	}
	return nil, &UnsupportedRuntimeError{SessionID: session, Runtime: rt, Reason: "no fake"}
}

func (s *fakeSessions) builds(session string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.built[session]
}
