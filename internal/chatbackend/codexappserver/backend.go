// Package codexappserver adapts an existing Codex app-server session to the chat
// wire. The owner supplies its stdout tap and existing turn/interrupt path.
package codexappserver

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/hollis-labs/libs/ui/ui-go/chatstream"
	decoder "github.com/hollis-labs/libs/ui/ui-go/chatstream/adapter/codexappserver"
	"github.com/hollis-labs/tether/internal/chatbackend"
)

type envelope struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params struct {
		ThreadID string `json:"threadId"`
		TurnID   string `json:"turnId"`
		Turn     struct {
			ID string `json:"id"`
		} `json:"turn"`
		Item struct {
			Type    string `json:"type"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"item"`
	} `json:"params"`
}

type Config struct {
	Tap *Tap
	// Send uses the owner's already initialized thread, never a second process.
	Send         func(context.Context, string) error
	Interrupt    func(context.Context, string, string) error
	CancelGrace  time.Duration
	ClaimTimeout time.Duration
}

type Backend struct {
	cfg       Config
	mu        sync.Mutex
	open      *run
	pending   *run
	notes     []note
	closed    bool
	ready     chan struct{}
	observed  chan chatbackend.ObservedRun
	observedQ []chatbackend.ObservedRun
	notify    chan struct{}
	bg        sync.WaitGroup
}

type note struct {
	text  string
	token any
}
type run struct {
	stream                        *chatbackend.Stream
	dec                           chatstream.Decoder
	thread, turn                  string
	text                          string
	observed, published, canceled bool
	deadline                      time.Time
}

func New(cfg Config) *Backend {
	if cfg.CancelGrace <= 0 {
		cfg.CancelGrace = 10 * time.Second
	}
	if cfg.ClaimTimeout <= 0 {
		cfg.ClaimTimeout = 10 * time.Minute
	}
	return &Backend{cfg: cfg, ready: make(chan struct{}), observed: make(chan chatbackend.ObservedRun), notify: make(chan struct{}, 1)}
}
func (*Backend) Info() chatbackend.Info                     { return chatbackend.Info{Deltas: true} }
func (b *Backend) Ready() <-chan struct{}                   { return b.ready }
func (b *Backend) Observed() <-chan chatbackend.ObservedRun { return b.observed }

func (b *Backend) Run(ctx context.Context) error {
	if b.cfg.Tap == nil {
		return errors.New("codexappserver: no session tap")
	}
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	un := b.cfg.Tap.subscribe(b.line, b.input)
	defer un()
	close(b.ready)
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	defer close(b.observed)
	defer b.bg.Wait()
	defer b.shutdown()
	sessionDone := b.cfg.Tap.Done()
	ended := false

	for {
		b.mu.Lock()
		if ended && len(b.observedQ) == 0 {
			b.mu.Unlock()
			return nil
		}
		var next chatbackend.ObservedRun
		var out chan chatbackend.ObservedRun
		if len(b.observedQ) > 0 {
			next = b.observedQ[0]
			out = b.observed
		}
		b.mu.Unlock()
		select {
		case out <- next:
			b.mu.Lock()
			b.observedQ = b.observedQ[1:]
			b.mu.Unlock()
		case <-b.notify:
		case <-tick.C:
			b.tick()
		case <-ctx.Done():
			return ctx.Err()
		case <-sessionDone:
			b.shutdown()
			ended = true
			sessionDone = nil
		}
	}
}

// shutdown completes buffered runs before Observed is drained on session EOF.
func (b *Backend) shutdown() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for _, r := range []*run{b.open, b.pending} {
		if r != nil {
			b.publish(r)
			r.end(errors.New("codex session ended"), false)
		}
	}
}

func (b *Backend) Submit(ctx context.Context, t chatbackend.Turn) (chatbackend.Run, error) {
	if strings.TrimSpace(t.Text) == "" {
		return nil, errors.New("codexappserver: empty turn")
	}
	text, err := t.InputText()
	if err != nil {
		return nil, err
	}
	if b.cfg.Send == nil || b.cfg.Tap == nil {
		return nil, errors.New("codexappserver: no session sender")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil, chatbackend.ErrClosed
	}
	if b.open != nil || b.pending != nil {
		b.mu.Unlock()
		return nil, chatbackend.ErrBusy
	}
	r := b.newRun(t.RunID)
	r.text = t.Text
	r.deadline = time.Now().Add(b.cfg.ClaimTimeout)
	b.pending = r
	b.mu.Unlock()
	err = b.cfg.Tap.Input(t.Text, r, true, func() error { return b.cfg.Send(ctx, text) })
	b.mu.Lock()
	defer b.mu.Unlock()
	if err != nil {
		if b.pending == r {
			b.pending = nil
		}
		r.end(err, false)
		return nil, err
	}
	return r.stream, nil
}

func (b *Backend) newRun(id string) *run {
	if id == "" {
		id = "run_" + uuid.NewString()
	}
	r := &run{dec: decoder.New().NewDecoder(chatstream.DecodeOptions{RunID: id, Provider: "codex"})}
	r.stream = chatbackend.NewStream(id, func() { b.cancel(r) })
	return r
}
func (r *run) feed(p []byte) {
	evs, err := r.dec.Decode(chatstream.Frame{Data: p})
	if err == nil {
		r.emit(evs, r.canceled)
	}
}
func (r *run) emit(evs []chatstream.Event, abort bool) {
	for i := range evs {
		if abort && evs[i].IsTerminal() {
			evs[i].Verb = chatstream.VerbRunAbort
			evs[i].Reason = "interrupted"
			evs[i].Code = ""
			evs[i].Message = ""
			evs[i].Retryable = false
		}
	}
	r.stream.Emit(evs...)
}
func (r *run) end(err error, abort bool) {
	if !r.stream.Done() {
		r.emit(r.dec.Close(err), abort || r.canceled)
	}
}
func (b *Backend) input(text string, token any, failed bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if failed {
		for i := len(b.notes) - 1; i >= 0; i-- {
			if b.notes[i].token == token {
				b.notes = append(b.notes[:i], b.notes[i+1:]...)
				break
			}
		}
		return
	}
	b.notes = append(b.notes, note{text, token})
}
func (b *Backend) publish(r *run) {
	if !r.observed || r.published {
		return
	}
	r.published = true
	b.observedQ = append(b.observedQ, chatbackend.ObservedRun{Run: r.stream, Text: publicText(r.text), Source: "app-server", TurnRef: r.turn})
	select {
	case b.notify <- struct{}{}:
	default:
	}
}
func (b *Backend) line(p []byte) {
	var e envelope
	if json.Unmarshal(p, &e) != nil || e.Method == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	if e.Method == "turn/started" {
		if b.open != nil && b.open.turn == e.Params.Turn.ID {
			return
		}
		if b.open != nil {
			b.publish(b.open)
			b.open.end(errors.New("turn replaced without completion"), false)
		}
		var r *run
		if len(b.notes) > 0 {
			n := b.notes[0]
			b.notes = b.notes[1:]
			if submitted, ok := n.token.(*run); ok && b.pending == submitted {
				r = submitted
				b.pending = nil
			} else {
				r = b.newRun("")
				r.observed = true
				r.text = n.text
			}
		} else {
			r = b.newRun("")
			r.observed = true
		}
		r.thread, r.turn = e.Params.ThreadID, e.Params.Turn.ID
		if !r.canceled {
			r.deadline = time.Time{}
		}
		b.open = r
		r.feed(p)
		if r.canceled {
			b.interrupt(r)
		}
		return
	}
	r := b.open
	if r == nil || (e.Params.ThreadID != "" && r.thread != "" && e.Params.ThreadID != r.thread) || (e.Params.TurnID != "" && e.Params.TurnID != r.turn) {
		return
	}
	// Never decode echoed user input: unknown decoder items retain raw payloads.
	// Submitted context is private even when Codex echoes it as a userMessage.
	if e.Params.Item.Type == "userMessage" {
		if r.observed && !r.published && r.text == "" {
			for _, c := range e.Params.Item.Content {
				r.text += c.Text
			}
		}
		b.publish(r)
		return
	}
	if e.Method == "turn/completed" && e.Params.Turn.ID != "" && e.Params.Turn.ID != r.turn {
		return
	}
	// Decode output and activity, never whole-thread snapshots or arbitrary
	// protocol notifications that can reflect the user's private input.
	if !outputMethod(e.Method) && len(e.ID) == 0 {
		return
	}
	b.publish(r)
	r.feed(p)
	if e.Method == "turn/completed" {
		if !r.stream.Done() {
			r.end(errors.New("turn ended without terminal event"), r.canceled)
		}
		b.open = nil
	}
}
func (b *Backend) cancel(r *run) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if r.stream.Done() || r.canceled {
		return
	}
	r.canceled = true
	r.deadline = time.Now().Add(b.cfg.CancelGrace)
	if b.open == r {
		b.interrupt(r)
	}
}

// interrupt never runs on the stdout callback: the RPC reply uses that reader.
func (b *Backend) interrupt(r *run) {
	if b.cfg.Interrupt == nil {
		return
	}
	b.bg.Add(1)
	go func() {
		defer b.bg.Done()
		ctx, stop := context.WithTimeout(context.Background(), b.cfg.CancelGrace)
		defer stop()
		_ = b.cfg.Interrupt(ctx, r.thread, r.turn)
	}()
}
func (b *Backend) tick() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, r := range []*run{b.open, b.pending} {
		if r == nil || r.stream.Done() || r.deadline.IsZero() || time.Now().Before(r.deadline) {
			continue
		}
		b.publish(r)
		r.end(errors.New("codex turn deadline exceeded"), r.canceled)
		// Keep the active tombstone until turn/completed: another Submit must not
		// interrupt a turn the provider still runs after the local cancel deadline.
		if b.pending == r && !r.canceled {
			b.pending = nil
		}
	}
}

func outputMethod(method string) bool {
	switch method {
	case "item/started", "item/completed", "item/agentMessage/delta", "item/reasoning/summaryTextDelta", "item/reasoning/summaryPartAdded", "thread/tokenUsage/updated", "turn/plan/updated", "turn/diff/updated", "error", "warning", "serverRequest/resolved", "turn/completed":
		return true
	default:
		return false
	}
}

// Context is transport-private even when a non-chat writer uses the same app
// envelope or a new backend attaches partway through a previously submitted turn.
func publicText(text string) string {
	if strings.HasPrefix(text, "<app-context>") {
		if _, rest, ok := strings.Cut(text, "</app-context>\n"); ok {
			return rest
		}
	}
	return text
}
