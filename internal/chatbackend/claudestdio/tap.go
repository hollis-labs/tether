package claudestdio

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
)

// Tap is one streaming-stdio session's two directions as the chat backend needs them: every stdout line (Write) and
// every user frame written to stdin (Input), in order. Tether owns one per Claude streaming-stdio session and feeds
// it from where it already reads the session: the runtime's output Fanout for a local session, accepted worker
// output frames for a worker session, and the SendTurn/SendInput paths for stdin. A Tap with no subscriber costs a
// newline scan per write.
//
// Claude's stdout does not echo the user frames it reads (without --replay-user-messages), so Input is how a backend
// learns who wrote the turn it sees start: the CLI runs each user frame as its own turn, in stdin order, and opens
// each with a system/init line. Input notes the frame to subscribers before it is written, under a lock that orders
// every writer, so the order of the notes is the order of the turns.
//
// A Tap is safe for concurrent use. Its subscribers are called synchronously from Write and Input and must not block
// or call back into the Tap.
type Tap struct {
	inMu  sync.Mutex // held across note + write, so the notes are in stdin order
	outMu sync.Mutex // held across a Write, so lines reach subscribers in order

	mu        sync.Mutex
	subs      map[int]Subscriber
	nextSub   int
	seq       uint64
	carry     []byte
	lineStart bool // the last byte written ended a line (or nothing was written)
	skip      bool // drop bytes up to the next newline: a subscriber joined mid-line
	closed    bool
	done      chan struct{}
}

// Subscriber receives what passes through a Tap. Any field may be nil.
type Subscriber struct {
	// Line is one stdout line, without its newline. The slice is the subscriber's to keep.
	Line func(line []byte)
	// Input is a user frame about to be written to stdin.
	Input func(InputNote)
	// InputFailed reports that the write of a noted frame failed: the CLI never read it.
	InputFailed func(InputNote)
}

// InputNote is one user frame written to the session's stdin.
type InputNote struct {
	// Seq numbers the notes of a Tap from 1.
	Seq uint64
	// Text is the frame's text content.
	Text string
	// From is what the writer passed to Input: a backend's own token, or a label such as "send_turn".
	From any
}

// NewTap returns an open Tap.
func NewTap() *Tap {
	return &Tap{subs: map[int]Subscriber{}, lineStart: true, done: make(chan struct{})}
}

// Subscribe adds s and returns the function that removes it. Lines already written are not replayed; if the stdout
// stream is mid-line, the rest of that line is skipped.
func (t *Tap) Subscribe(s Subscriber) (cancel func()) {
	t.outMu.Lock()
	defer t.outMu.Unlock()
	t.mu.Lock()
	defer t.mu.Unlock()
	id := t.nextSub
	t.nextSub++
	if len(t.subs) == 0 {
		t.carry = nil
		t.skip = !t.lineStart
	}
	t.subs[id] = s
	return func() {
		t.mu.Lock()
		delete(t.subs, id)
		t.mu.Unlock()
	}
}

// Done is closed by Close.
func (t *Tap) Done() <-chan struct{} { return t.done }

// Close marks the session's output ended. Write and Input keep working (Input still writes) but notify no one.
func (t *Tap) Close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	t.closed = true
	t.subs = map[int]Subscriber{}
	close(t.done)
}

func (t *Tap) snapshot() []Subscriber {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.subs) == 0 {
		return nil
	}
	out := make([]Subscriber, 0, len(t.subs))
	for _, s := range t.subs {
		out = append(out, s)
	}
	return out
}

// Write takes a chunk of the session's stdout (any split, not necessarily whole lines). It always succeeds, so it
// can sit in an io.MultiWriter next to the session log.
func (t *Tap) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	t.outMu.Lock()
	defer t.outMu.Unlock()
	t.mu.Lock()
	t.lineStart = p[len(p)-1] == '\n'
	if len(t.subs) == 0 {
		t.mu.Unlock()
		return len(p), nil
	}
	data := p
	if t.skip {
		at := bytes.IndexByte(data, '\n')
		if at < 0 {
			t.mu.Unlock()
			return len(p), nil
		}
		data, t.skip = data[at+1:], false
	}
	var lines [][]byte
	buf := append(t.carry, data...)
	for {
		at := bytes.IndexByte(buf, '\n')
		if at < 0 {
			break
		}
		if line := bytes.TrimRight(buf[:at], "\r"); len(bytes.TrimSpace(line)) > 0 {
			lines = append(lines, append([]byte(nil), line...))
		}
		buf = buf[at+1:]
	}
	t.carry = append([]byte(nil), buf...)
	t.mu.Unlock()
	if len(lines) == 0 {
		return len(p), nil
	}
	subs := t.snapshot()
	for _, line := range lines {
		for _, s := range subs {
			if s.Line != nil {
				s.Line(append([]byte(nil), line...))
			}
		}
	}
	return len(p), nil
}

// Input writes data to the session's stdin through write, noting each user frame in it to subscribers first. data
// is what write sends: one or more newline-separated lines; lines that are not user text frames (control requests,
// tool results, raw bytes) pass without a note. from identifies the writer in the notes. Writers are serialized.
func (t *Tap) Input(data []byte, from any, write func() error) error {
	t.inMu.Lock()
	defer t.inMu.Unlock()
	subs := t.snapshot()
	if len(subs) == 0 {
		return write()
	}
	var notes []InputNote
	for _, line := range bytes.Split(data, []byte("\n")) {
		if text, ok := UserFrameText(line); ok {
			t.mu.Lock()
			t.seq++
			n := InputNote{Seq: t.seq, Text: text, From: from}
			t.mu.Unlock()
			notes = append(notes, n)
		}
	}
	for _, n := range notes {
		for _, s := range subs {
			if s.Input != nil {
				s.Input(n)
			}
		}
	}
	err := write()
	if err != nil {
		for _, n := range notes {
			for _, s := range subs {
				if s.InputFailed != nil {
					s.InputFailed(n)
				}
			}
		}
	}
	return err
}

// UserFrame is the stream-json frame that submits text as one user turn (the shape internal/app's SendTurn writes):
// {"type":"user","message":{"role":"user","content":text}}, without a newline.
func UserFrame(text string) []byte {
	return mustJSON(map[string]any{"type": "user", "message": map[string]string{"role": "user", "content": text}})
}

// InterruptFrame is the stream-json control request that interrupts the turn in flight, without a newline. Claude
// answers it with a control_response (success whether or not a turn runs); a running turn then ends with an
// error_during_execution result whose terminal_reason is aborted_streaming or aborted_tools, and the process stays
// up for the next turn. Recorded with Claude Code 2.1.327 (testdata/interrupt).
func InterruptFrame(requestID string) []byte {
	return mustJSON(map[string]any{"type": "control_request", "request_id": requestID, "request": map[string]string{"subtype": "interrupt"}})
}

// UserFrameText is the text of a stream-json user frame that starts a turn: the content string, or its text blocks
// joined by newlines. ok is false for anything else, including a user frame that carries only tool results.
func UserFrameText(line []byte) (text string, ok bool) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 || line[0] != '{' {
		return "", false
	}
	var f struct {
		Type    string `json:"type"`
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &f) != nil || f.Type != "user" || len(f.Message.Content) == 0 {
		return "", false
	}
	if json.Unmarshal(f.Message.Content, &text) == nil {
		return text, true
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(f.Message.Content, &blocks) != nil {
		return "", false
	}
	var parts []string
	other := false
	for _, b := range blocks {
		switch b.Type {
		case "text":
			parts = append(parts, b.Text)
		case "tool_result":
		default:
			other = true // an image or document: still a turn
		}
	}
	if len(parts) == 0 && !other {
		return "", false
	}
	return strings.Join(parts, "\n"), true
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
