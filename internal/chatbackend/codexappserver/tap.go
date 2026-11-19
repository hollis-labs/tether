package codexappserver

import (
	"encoding/json"
	"sync"

	"github.com/hollis-labs/tether/internal/chatbackend"
	"github.com/hollis-labs/tether/internal/chatbackend/claudestdio"
)

// Tap copies the existing session's JSON-RPC stdout. It never owns a process or
// answers server requests. Input orders writers before their notifications arrive.
type Tap struct {
	lines      *claudestdio.Tap
	inputMu    sync.Mutex
	mu         sync.Mutex
	subscriber func([]byte)
	note       func(string, any, bool)
	active     []byte
	inputs     []tapInput
	nextInput  uint64
	activeText string
	closed     bool
}

type tapInput struct {
	seq  uint64
	text string
}

func NewTap() *Tap {
	t := &Tap{lines: claudestdio.NewTap()}
	t.lines.Subscribe(claudestdio.Subscriber{Line: t.line})
	return t
}

func (t *Tap) Write(p []byte) (int, error) { return t.lines.Write(p) }
func (t *Tap) Done() <-chan struct{}       { return t.lines.Done() }
func (t *Tap) Close()                      { t.mu.Lock(); t.closed = true; t.mu.Unlock(); t.lines.Close() }

func (t *Tap) line(p []byte) {
	var e envelope
	if json.Unmarshal(p, &e) != nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	switch e.Method {
	case "turn/started":
		t.active = append([]byte(nil), p...)
		t.activeText = ""
		if len(t.inputs) > 0 {
			t.activeText = t.inputs[0].text
			t.inputs = t.inputs[1:]
		}
	case "turn/completed":
		t.active = nil
		t.activeText = ""
	}
	if t.subscriber != nil {
		t.subscriber(p)
	}
}

// subscribe also reports an already active turn, so attaching midway cannot
// admit another Submit. Only public input notes and the turn boundary are
// replayed; the backend never decodes echoed user frames.
func (t *Tap) subscribe(line func([]byte), note func(string, any, bool)) func() {
	t.mu.Lock()
	t.subscriber, t.note = line, note
	if len(t.active) != 0 {
		if t.activeText != "" {
			note(t.activeText, "reattach", false)
		}
		line(t.active)
	}
	for _, n := range t.inputs {
		note(n.text, "reattach_pending", false)
	}
	t.mu.Unlock()
	return func() { t.mu.Lock(); t.subscriber, t.note = nil, nil; t.mu.Unlock() }
}

// Input notes a public input before sending it. exclusive is used only by the
// chat backend: external callers retain their existing delivery behavior.
func (t *Tap) Input(text string, token any, exclusive bool, send func() error) error {
	t.inputMu.Lock()
	defer t.inputMu.Unlock()
	t.mu.Lock()
	if exclusive && (len(t.inputs) > 0 || len(t.active) != 0 || t.closed) {
		err := chatbackend.ErrBusy
		if t.closed {
			err = chatbackend.ErrClosed
		}
		t.mu.Unlock()
		return err
	}
	t.nextInput++
	seq := t.nextInput
	t.inputs = append(t.inputs, tapInput{seq, text})
	if t.note != nil {
		t.note(text, token, false)
	}
	t.mu.Unlock()
	err := send()
	if err != nil {
		t.mu.Lock()
		for i, n := range t.inputs {
			if n.seq == seq {
				t.inputs = append(t.inputs[:i], t.inputs[i+1:]...)
				break
			}
		}
		if t.note != nil {
			t.note(text, token, true)
		}
		t.mu.Unlock()
	}
	return err
}
