// Package claudestdio is the chat backend for a Tether session running Claude Code in streaming-stdio mode
// (`claude -p --input-format stream-json --output-format stream-json --verbose --include-partial-messages`;
// CW-20261118-0010). It is the second chatbackend.Backend, next to claudepty.
//
// The CLI keeps one process across turns. Each user frame on stdin becomes one turn, run in stdin order (a frame
// written while a turn runs waits and runs next as its own turn); the turn's stdout opens with a system/init line and
// ends with one result line. So:
//
//   - Content: stdout lines from system/init through result, decoded with chatstream/adapter/claudejson (text,
//     reasoning and tool-argument deltas, whole tool results), then through a Finalizer that fills part.end's Final
//     (claudejson does not on text and reasoning). Runs carry part.delta (Info.Deltas true).
//   - Submit writes a user frame; Cancel writes the interrupt control request, and the CLI ends the turn with an
//     aborted result, which claudejson maps to run.abort.
//   - Observed runs: stdout does not echo user frames, so the backend learns who wrote each turn from the Tap, which
//     notes every user frame Tether writes (its own Submits and SendTurn/SendInput: mail wakes, MCP, the CLI) in stdin
//     order. Each system/init takes the oldest noted frame: a Submit's is that run, any other is an observed run
//     carrying the frame's text. A turn with no noted frame (a frame Tether did not write through the Tap, such as a
//     boot prompt the runtime wrote itself) is still reported, as an observed run with empty text and Source
//     SourceUnseen.
package claudestdio

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hollis-labs/libs/ui/ui-go/chatstream"
	"github.com/hollis-labs/libs/ui/ui-go/chatstream/adapter/claudejson"

	"github.com/hollis-labs/tether/internal/chatbackend"
)

// Stdin writes one stream-json frame (no trailing newline) to the session's stdin. The backend calls it inside
// Tap.Input, so an implementation must write directly and not note the frame to the Tap again.
type Stdin interface {
	Write(ctx context.Context, frame []byte) error
}

// StdinFunc adapts a function to Stdin.
type StdinFunc func(ctx context.Context, frame []byte) error

// Write implements Stdin.
func (f StdinFunc) Write(ctx context.Context, frame []byte) error { return f(ctx, frame) }

// Defaults for Config's durations.
const (
	DefaultCancelGrace  = 10 * time.Second
	DefaultClaimTimeout = 10 * time.Minute
	tickInterval        = 100 * time.Millisecond
)

// Observed-run sources.
const (
	// SourceStdin: the turn's user frame was written through the Tap by someone other than this backend. Origin is the
	// writer's label (internal/app: "send_turn", "send_input").
	SourceStdin = "stdin"
	// SourceUnseen: the turn started with no user frame noted for it; its text is unknown.
	SourceUnseen = "unseen"
)

// Run error codes this backend produces (beyond the decoder's).
const (
	// CodeInputNotSeen: the CLI did not start the submitted turn within ClaimTimeout (the turns queued ahead of it ran
	// long, or the frame was lost). If it starts later it is reported as an observed run.
	CodeInputNotSeen = "input_not_seen"
	// CodeSessionEnded: the session's output ended during the turn.
	CodeSessionEnded = "session_ended"
)

// Config configures a Backend.
type Config struct {
	// Tap carries the session's stdout and notes its stdin. Required.
	Tap *Tap
	// Stdin writes to the session. Required for Submit and Cancel.
	Stdin Stdin
	// Model goes on run.start when the init line has none.
	Model string
	// NewFinalizer returns a run's Finalizer. Default: chatbackend.NewFinalizer, keeping deltas.
	NewFinalizer func() chatbackend.Finalizer
	// CancelGrace is how long after the interrupt request the run waits for the CLI's aborted result before it aborts
	// anyway.
	CancelGrace time.Duration
	// ClaimTimeout is how long a submitted turn may wait for the CLI to start it.
	ClaimTimeout time.Duration
	// Now and NewRunID are for tests.
	Now      func() time.Time
	NewRunID func() string
}

// Backend is the Claude streaming-stdio chat backend. It implements chatbackend.Backend.
type Backend struct {
	cfg Config

	mu         sync.Mutex
	queue      []*input // user frames written to stdin whose turn has not started, in stdin order
	submitting *input   // a Submit whose frame is being written and has not been noted yet
	open       *turn    // the turn the CLI is running
	interrupts int
	closed     bool
	observedQ  []chatbackend.ObservedRun
	observedN  chan struct{}
	observed   chan chatbackend.ObservedRun
	startOnce  sync.Once
	bg         sync.WaitGroup // interrupts sent off the stdout path
	ready      chan struct{}  // closed once Run follows the Tap
	readyOnce  sync.Once
}

// input is one user frame the CLI will run as a turn.
type input struct {
	seq      uint64
	text     string
	origin   string
	run      *run // the submitted run; nil for another writer's frame or one whose run already ended
	deadline time.Time
	canceled bool // interrupt the turn as soon as it starts
}

type run struct {
	stream  *chatbackend.Stream
	dec     chatstream.Decoder
	fin     chatbackend.Finalizer
	started bool
}

type turn struct {
	run      *run // nil: the turn's run already ended (forced abort); its lines are dropped
	cancelAt time.Time
	// streaming: a streamed assistant message is open (stream_event message_start seen, message_stop not yet).
	// held: user lines (tool results) that arrived inside it. With parallel tool calls the CLI writes the first
	// call's result while the model is still streaming the next call's arguments; claudejson would open the result's
	// user message inside the assistant one, so those lines wait for message_stop.
	streaming bool
	held      [][]byte
}

// feed passes a line of the turn to its run, holding user lines while a streamed message is open.
func (t *turn) feed(line []byte, typ string, event string) {
	if t.run == nil {
		return
	}
	switch {
	case typ == "user" && t.streaming:
		t.held = append(t.held, line)
		return
	case typ == "stream_event" && event == "message_start":
		t.flush()
		t.streaming = true
	case typ == "result":
		t.flush()
	}
	t.run.feed(line)
	if typ == "stream_event" && event == "message_stop" {
		t.streaming = false
		t.flush()
	}
}

func (t *turn) flush() {
	held := t.held
	t.held, t.streaming = nil, false
	for _, l := range held {
		t.run.feed(l)
	}
}

var _ chatbackend.Backend = (*Backend)(nil)

// New returns a Backend. Start it with Run.
func New(cfg Config) *Backend {
	if cfg.NewFinalizer == nil {
		cfg.NewFinalizer = func() chatbackend.Finalizer { return chatbackend.NewFinalizer(chatbackend.FinalizerOptions{}) }
	}
	if cfg.CancelGrace <= 0 {
		cfg.CancelGrace = DefaultCancelGrace
	}
	if cfg.ClaimTimeout <= 0 {
		cfg.ClaimTimeout = DefaultClaimTimeout
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.NewRunID == nil {
		cfg.NewRunID = newRunID
	}
	return &Backend{cfg: cfg, observedN: make(chan struct{}, 1), observed: make(chan chatbackend.ObservedRun), ready: make(chan struct{})}
}

// Info implements chatbackend.Backend.
func (b *Backend) Info() chatbackend.Info { return chatbackend.Info{Deltas: true, Approvals: false} }

// Ready is closed once Run follows the Tap. Stdout the CLI writes before then reaches no backend, so a caller that
// starts Run and submits at once waits for it (CW-20261118-0011).
func (b *Backend) Ready() <-chan struct{} { return b.ready }

// Observed implements chatbackend.Backend.
func (b *Backend) Observed() <-chan chatbackend.ObservedRun { return b.observed }

// subscriber is what the backend hands the Tap.
func (b *Backend) subscriber() Subscriber {
	return Subscriber{Line: b.onLine, Input: b.onInput, InputFailed: b.onInputFailed}
}

// Run follows the Tap until ctx ends or the Tap closes (the session's output ended), then ends every open run and
// closes Observed. It returns ctx's error, or nil when the session ended.
func (b *Backend) Run(ctx context.Context) error {
	if b.cfg.Tap == nil {
		return errors.New("claudestdio: no tap")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	b.startOnce.Do(func() { go b.forwardObserved(ctx) })
	unsubscribe := b.cfg.Tap.Subscribe(b.subscriber())
	defer unsubscribe()
	b.readyOnce.Do(func() { close(b.ready) })
	tick := time.NewTicker(tickInterval)
	defer tick.Stop()
	var err error
	ended := false
loop:
	for {
		select {
		case <-ctx.Done():
			err = ctx.Err()
			break loop
		case <-b.cfg.Tap.Done():
			ended = true
			break loop
		case <-tick.C:
			b.tick(b.cfg.Now())
		}
	}
	b.shutdown(err, ended)
	b.bg.Wait()
	return err
}

// Submit implements chatbackend.Backend: it writes the text as one stream-json user frame.
func (b *Backend) Submit(ctx context.Context, t chatbackend.Turn) (chatbackend.Run, error) {
	if strings.TrimSpace(t.Text) == "" {
		return nil, errors.New("claudestdio: empty turn")
	}
	if b.cfg.Stdin == nil || b.cfg.Tap == nil {
		return nil, errors.New("claudestdio: no stdin")
	}
	b.mu.Lock()
	switch {
	case b.closed:
		b.mu.Unlock()
		return nil, chatbackend.ErrClosed
	case b.open != nil || len(b.queue) > 0 || b.submitting != nil:
		// The CLI would queue the frame and run it after the others; refused so a chat has one turn in flight, as
		// with the PTY backend.
		b.mu.Unlock()
		return nil, chatbackend.ErrBusy
	}
	id := t.RunID
	if id == "" {
		id = b.cfg.NewRunID()
	}
	r := b.newRun(id)
	in := &input{text: t.Text, run: r, deadline: b.cfg.Now().Add(b.cfg.ClaimTimeout)}
	b.submitting = in
	b.mu.Unlock()

	frame := UserFrame(t.Text)
	err := b.cfg.Tap.Input(frame, in, func() error { return b.cfg.Stdin.Write(ctx, frame) })

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.submitting == in {
		// Not noted (the Tap had closed, or nobody was subscribed because Run has not started): queue it now.
		b.submitting = nil
		if err == nil {
			if in.canceled {
				r.abort()
				in.run = nil
			}
			b.queue = append(b.queue, in)
		}
	}
	if err != nil {
		b.remove(in)
		r.fail(err)
		return nil, fmt.Errorf("claudestdio: write user frame: %w", err)
	}
	if b.closed && in.run != nil && !r.stream.Done() {
		// Run ended while the frame was being written.
		r.fail(chatbackend.ErrClosed)
	}
	return r.stream, nil
}

func (b *Backend) newRun(id string) *run {
	r := &run{
		dec: claudejson.New().NewDecoder(chatstream.DecodeOptions{RunID: id, Provider: "claude", Model: b.cfg.Model, Now: b.cfg.Now}),
		fin: b.cfg.NewFinalizer(),
	}
	r.stream = chatbackend.NewStream(id, func() { b.cancel(r) })
	return r
}

// initFrame opens a run that ends before the CLI started it.
var initFrame = []byte(`{"type":"system","subtype":"init"}`)

func (r *run) feed(frame []byte) {
	evs, err := r.dec.Decode(chatstream.Frame{Data: frame})
	if err != nil {
		return
	}
	r.started = true
	r.emit(evs)
}

func (r *run) start() {
	if !r.started {
		r.feed(initFrame)
	}
}

func (r *run) emit(evs []chatstream.Event) {
	for _, ev := range evs {
		r.stream.Emit(r.fin.Push(ev)...)
	}
}

// fail ends the run with run.error (the decoder closes what is open first).
func (r *run) fail(cause error) { r.start(); r.emit(r.dec.Close(cause)) }

// abort ends the run with run.abort.
func (r *run) abort() {
	r.start()
	r.feed([]byte(`{"type":"result","subtype":"error_during_execution","is_error":true,"terminal_reason":"aborted_streaming"}`))
}

// errorf ends the run with run.error carrying code (the decoder is closed first, so no part stays open).
func (r *run) errorf(now time.Time, code, format string, args ...any) {
	r.start()
	for _, ev := range r.dec.Close(nil) {
		if ev.IsTerminal() {
			ev.Code, ev.Message, ev.Retryable = code, fmt.Sprintf(format, args...), false
		}
		r.stream.Emit(r.fin.Push(ev)...)
	}
	if !r.stream.Done() {
		r.stream.Emit(chatstream.Event{V: chatstream.SchemaVersion, Verb: chatstream.VerbRunError, Time: now, Code: code, Message: fmt.Sprintf(format, args...)})
	}
}

// cancel is Run.Cancel.
func (b *Backend) cancel(r *run) {
	b.mu.Lock()
	switch {
	case b.open != nil && b.open.run == r:
		send := b.open.cancelAt.IsZero()
		if send {
			b.open.cancelAt = b.cfg.Now()
		}
		b.mu.Unlock()
		if send {
			b.interrupt()
		}
		return
	case b.submitting != nil && b.submitting.run == r:
		b.submitting.canceled = true // onInput or Submit queues it; canceled there
	default:
		for _, in := range b.queue {
			if in.run == r {
				// The CLI has the frame and will run it: end the run now and interrupt the turn when it starts.
				in.canceled, in.run = true, nil
				r.abort()
				break
			}
		}
	}
	b.mu.Unlock()
}

// interrupt writes the interrupt control request. b.mu must not be held.
func (b *Backend) interrupt() {
	if b.cfg.Stdin == nil || b.cfg.Tap == nil {
		return
	}
	b.mu.Lock()
	b.interrupts++
	id := "chat_interrupt_" + strconv.Itoa(b.interrupts)
	b.mu.Unlock()
	frame := InterruptFrame(id)
	// A failed write ends in CancelGrace's forced abort.
	_ = b.cfg.Tap.Input(frame, b, func() error { return b.cfg.Stdin.Write(context.Background(), frame) })
}

// onInput takes a user frame some writer is about to put on stdin.
func (b *Backend) onInput(n InputNote) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if in, ok := n.From.(*input); ok && in == b.submitting {
		b.submitting = nil
		in.seq = n.Seq
		if in.canceled && in.run != nil {
			in.run.abort()
			in.run = nil
		}
		b.queue = append(b.queue, in)
		return
	}
	in := &input{seq: n.Seq, text: n.Text}
	if s, ok := n.From.(string); ok {
		in.origin = s
	}
	b.queue = append(b.queue, in)
}

// onInputFailed drops a frame the CLI never got.
func (b *Backend) onInputFailed(n InputNote) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i, in := range b.queue {
		if in.seq == n.Seq {
			b.queue = append(b.queue[:i], b.queue[i+1:]...)
			return
		}
	}
}

// remove drops in from the queue; b.mu is held.
func (b *Backend) remove(in *input) {
	for i, q := range b.queue {
		if q == in {
			b.queue = append(b.queue[:i], b.queue[i+1:]...)
			return
		}
	}
}

// onLine consumes one stdout line.
func (b *Backend) onLine(line []byte) {
	var head struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
		UUID    string `json:"uuid"`
		Event   struct {
			Type string `json:"type"`
		} `json:"event"`
	}
	if json.Unmarshal(line, &head) != nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.cfg.Now()
	switch {
	case head.Type == "system" && head.Subtype == "init":
		if b.open != nil && b.open.run != nil {
			// A new turn without the last one's result: the CLI lost it.
			b.open.flush()
			b.open.run.fail(nil)
		}
		b.open = nil
		var in *input
		if len(b.queue) > 0 {
			in = b.queue[0]
			b.queue = b.queue[1:]
		}
		t := &turn{}
		switch {
		case in != nil && in.canceled:
			// A submitted turn canceled while it waited: its run already ended with run.abort. The turn runs unread
			// until the interrupt lands. The write is not on the stdout path: a worker's input is a network call.
			t.cancelAt = now
			b.bg.Add(1)
			go func() { defer b.bg.Done(); b.interrupt() }()
		case in != nil && in.run != nil:
			t.run = in.run
		case in != nil:
			t.run = b.newRun(b.cfg.NewRunID())
			b.publishObserved(chatbackend.ObservedRun{Run: t.run.stream, Text: in.text, Source: SourceStdin, Origin: in.origin, TurnRef: head.UUID})
		default:
			t.run = b.newRun(b.cfg.NewRunID())
			b.publishObserved(chatbackend.ObservedRun{Run: t.run.stream, Source: SourceUnseen, TurnRef: head.UUID})
		}
		if t.run != nil {
			t.run.feed(line)
		}
		b.open = t
	case head.Type == "control_response", head.Type == "control_request", head.Type == "keep_alive":
		// Answers to interrupts, and permission prompts this backend does not bridge (Info.Approvals false).
	case b.open == nil:
		// Between turns: nothing to attach it to.
	case head.Type == "result":
		b.open.feed(line, head.Type, "")
		b.open = nil
	default:
		b.open.feed(line, head.Type, head.Event.Type)
	}
}

// tick enforces the timers.
func (b *Backend) tick(now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, in := range b.queue {
		if in.run != nil && !now.Before(in.deadline) {
			in.run.errorf(now, CodeInputNotSeen, "the agent did not start the submitted turn within %s", b.cfg.ClaimTimeout)
			in.run = nil // if it starts later, it is an observed run
		}
	}
	if t := b.open; t != nil && t.run != nil && !t.cancelAt.IsZero() && !now.Before(t.cancelAt.Add(b.cfg.CancelGrace)) {
		t.flush()
		t.run.abort()
		t.run = nil // the rest of the turn, if it comes, is dropped
	}
}

// shutdown ends every open run; nothing is submitted afterwards. ended reports that the session's output ended.
func (b *Backend) shutdown(cause error, ended bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	now := b.cfg.Now()
	end := func(r *run) {
		if ended {
			r.errorf(now, CodeSessionEnded, "the Claude session ended during the turn")
			return
		}
		if cause == nil {
			cause = chatbackend.ErrClosed
		}
		r.fail(cause)
	}
	if b.open != nil && b.open.run != nil {
		b.open.flush()
		end(b.open.run)
	}
	b.open = nil
	for _, in := range b.queue {
		if in.run != nil {
			end(in.run)
		}
	}
	b.queue = nil
	if b.submitting != nil && b.submitting.run != nil {
		end(b.submitting.run)
		b.submitting.run = nil
	}
}

// publishObserved queues an observed run for Observed; b.mu is held. The queue is unbounded so the stdout path never
// waits on the consumer.
func (b *Backend) publishObserved(o chatbackend.ObservedRun) {
	b.observedQ = append(b.observedQ, o)
	select {
	case b.observedN <- struct{}{}:
	default:
	}
}

func (b *Backend) forwardObserved(ctx context.Context) {
	defer close(b.observed)
	for {
		b.mu.Lock()
		q := b.observedQ
		b.observedQ = nil
		b.mu.Unlock()
		for _, o := range q {
			select {
			case b.observed <- o:
			case <-ctx.Done():
				return
			}
		}
		if len(q) == 0 {
			select {
			case <-b.observedN:
			case <-ctx.Done():
				return
			}
		}
	}
}

func newRunID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "run_" + hex.EncodeToString(b[:])
}
