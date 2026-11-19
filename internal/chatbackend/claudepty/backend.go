// Package claudepty is the chat backend for a Tether session running the Claude Code TUI in a PTY (CW-20261118-0010).
//
// The TUI has no structured output, so a run is assembled from two other places:
//
//   - Content from the vendor transcript (claudetranscript): whole messages, mapped to claudejson frames and decoded
//     with chatstream/adapter/claudejson, then through a Finalizer that drops the decoder's one-per-block part.delta
//     and fills part.end's Final. Runs carry no part.delta (Info.Deltas false).
//   - Turn boundaries from Tether's Claude hooks (HookSource, normally the hook spool): UserPromptSubmit says a turn
//     is open before its prompt line reaches the transcript, Stop that it is over. The transcript's own end markers
//     (stop_hook_summary, turn_duration, the interrupt line) close the run; Stop arms a grace timer for when they do
//     not arrive, because the turn's last assistant line can land after Stop fires.
//
// Submit pastes the text (bracketed paste) and presses Enter; Cancel presses Esc. A turn whose prompt matches a pending
// Submit belongs to that run. Every other turn (typed into the terminal, a mail wake, a plugin message) is an observed
// run carrying the prompt text.
package claudepty

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/hollis-labs/libs/ui/ui-go/chatstream"
	"github.com/hollis-labs/libs/ui/ui-go/chatstream/adapter/claudejson"

	"github.com/hollis-labs/tether/internal/chatbackend"
	"github.com/hollis-labs/tether/internal/chatbackend/claudetranscript"
)

// LineSource yields transcript lines in file order until ctx ends. jsonltail.Tail and SessionTranscript satisfy it.
type LineSource interface {
	Run(ctx context.Context, emit func(line []byte)) error
}

// Defaults for Config's durations.
const (
	DefaultFlushGrace   = 3 * time.Second
	DefaultCancelGrace  = 5 * time.Second
	DefaultClaimTimeout = 2 * time.Minute
	DefaultEnterDelay   = 60 * time.Millisecond
	tickInterval        = 100 * time.Millisecond
)

// Activity kinds this backend adds to a run.
const (
	// ActivityQueuedInput is on the open run when the TUI folded input typed (or pasted) mid-turn into it. Value:
	// {"text": ...}.
	ActivityQueuedInput = "claude.queued_input"
)

// Run error codes this backend produces (beyond the decoder's).
const (
	// CodeInputMerged: the submitted text reached the TUI while a turn ran, and the TUI folded it into that turn.
	CodeInputMerged = "input_merged"
	// CodeInputNotSeen: no turn with the submitted text started within ClaimTimeout.
	CodeInputNotSeen = "input_not_seen"
	// CodeSessionEnded: the Claude session ended (SessionEnd hook) during the turn.
	CodeSessionEnded = "session_ended"
)

// Config configures a Backend.
type Config struct {
	// SessionID is the Tether session the PTY belongs to: the id passed to Input.
	SessionID string
	// Input writes to the session's PTY.
	Input Input
	// Transcript yields the Claude transcript's lines. Required.
	Transcript LineSource
	// Hooks yields the session's Claude hook signals. Optional: without it a run ends on the transcript's markers
	// alone, and Submit cannot see a turn that started but has not reached the transcript yet.
	Hooks HookSource
	// ClaudeSessionID is the vendor session id when the launcher knows it (claude --session-id); hooks supply it
	// otherwise.
	ClaudeSessionID string
	// Model goes on run.start when set.
	Model string
	// NewFinalizer returns a run's Finalizer. Default: chatbackend.NewFinalizer with DropDeltas.
	NewFinalizer func() chatbackend.Finalizer
	// FlushGrace is how long after Stop the run waits for the transcript's end marker before it ends anyway.
	FlushGrace time.Duration
	// CancelGrace is how long after Esc the run waits for the transcript's interrupt line before it aborts anyway.
	CancelGrace time.Duration
	// ClaimTimeout is how long a submitted text may take to show up as a turn.
	ClaimTimeout time.Duration
	// EnterDelay separates the paste from Enter, so the TUI has taken the paste before the key arrives.
	EnterDelay time.Duration
	// Now and NewRunID are for tests.
	Now      func() time.Time
	NewRunID func() string
}

// Backend is the Claude PTY chat backend. It implements chatbackend.Backend.
type Backend struct {
	cfg Config

	mu      sync.Mutex
	mapper  claudetranscript.Mapper
	open    *turn    // the turn the transcript has open
	pending *pending // a submitted text not yet seen as a turn
	// Keep public text for context-bearing prompts even after a claim times out:
	// a late transcript or queued-input echo must not publish private context.
	publicPrompts map[string]string
	hookOpen      string // prompt id of a turn the hooks opened and have not closed
	stopped       map[string]time.Time
	claudeSID     string
	closed        bool
	observedQ     []chatbackend.ObservedRun
	observedN     chan struct{}
	observed      chan chatbackend.ObservedRun
	startOnce     sync.Once
}

type run struct {
	stream *chatbackend.Stream
	dec    chatstream.Decoder
	fin    chatbackend.Finalizer
}

type turn struct {
	promptID string
	run      *run
	stopAt   time.Time // the Stop hook fired
	cancelAt time.Time // Esc was sent
}

type pending struct {
	text     string
	run      *run
	deadline time.Time
	canceled bool
}

var _ chatbackend.Backend = (*Backend)(nil)

// New returns a Backend. Start it with Run.
func New(cfg Config) *Backend {
	if cfg.NewFinalizer == nil {
		cfg.NewFinalizer = func() chatbackend.Finalizer {
			return chatbackend.NewFinalizer(chatbackend.FinalizerOptions{DropDeltas: true})
		}
	}
	if cfg.FlushGrace <= 0 {
		cfg.FlushGrace = DefaultFlushGrace
	}
	if cfg.CancelGrace <= 0 {
		cfg.CancelGrace = DefaultCancelGrace
	}
	if cfg.ClaimTimeout <= 0 {
		cfg.ClaimTimeout = DefaultClaimTimeout
	}
	if cfg.EnterDelay <= 0 {
		cfg.EnterDelay = DefaultEnterDelay
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.NewRunID == nil {
		cfg.NewRunID = newRunID
	}
	return &Backend{
		cfg:           cfg,
		stopped:       map[string]time.Time{},
		publicPrompts: map[string]string{},
		claudeSID:     cfg.ClaudeSessionID,
		observedN:     make(chan struct{}, 1),
		observed:      make(chan chatbackend.ObservedRun),
	}
}

// Info implements chatbackend.Backend.
func (b *Backend) Info() chatbackend.Info { return chatbackend.Info{Deltas: false, Approvals: false} }

// Observed implements chatbackend.Backend.
func (b *Backend) Observed() <-chan chatbackend.ObservedRun { return b.observed }

// ClaudeSessionID is the vendor session id: Config's, or the latest a hook reported. SessionTranscript reads it.
func (b *Backend) ClaudeSessionID() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.claudeSID
}

// Run follows the transcript and the hooks until ctx ends, then ends every open run with run.error and closes
// Observed. It returns ctx's error, or a source's error.
func (b *Backend) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	b.startOnce.Do(func() { go b.forwardObserved(ctx) })
	errs := make(chan error, 3)
	go func() { errs <- b.cfg.Transcript.Run(ctx, b.onLine) }()
	n := 1
	if b.cfg.Hooks != nil {
		n++
		go func() { errs <- b.cfg.Hooks.Run(ctx, b.onHook) }()
	}
	tick := time.NewTicker(tickInterval)
	defer tick.Stop()
	var err error
loop:
	for {
		select {
		case <-ctx.Done():
			err = ctx.Err()
			break loop
		case e := <-errs:
			n--
			if e != nil && !errors.Is(e, context.Canceled) {
				err = e
				break loop
			}
			if n == 0 {
				err = ctx.Err()
				break loop
			}
		case <-tick.C:
			b.tick(b.cfg.Now())
		}
	}
	cancel()
	b.shutdown(err)
	return err
}

// Submit implements chatbackend.Backend: it pastes the text into the TUI and presses Enter.
func (b *Backend) Submit(ctx context.Context, t chatbackend.Turn) (chatbackend.Run, error) {
	if cleanText(t.Text) == "" {
		return nil, errors.New("claudepty: empty turn")
	}
	if b.cfg.Input == nil {
		return nil, errors.New("claudepty: no input")
	}
	inputText, err := t.InputText()
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	switch {
	case b.closed:
		b.mu.Unlock()
		return nil, chatbackend.ErrClosed
	case b.open != nil || b.hookOpen != "" || b.pending != nil:
		b.mu.Unlock()
		return nil, chatbackend.ErrBusy
	}
	id := t.RunID
	if id == "" {
		id = b.cfg.NewRunID()
	}
	r := b.newRun(id)
	p := &pending{text: inputText, run: r, deadline: b.cfg.Now().Add(b.cfg.ClaimTimeout)}
	b.pending = p
	if len(t.Context) != 0 {
		b.publicPrompts[promptKey(inputText)] = t.Text
	}
	b.mu.Unlock()

	err = b.cfg.Input.SendInput(b.cfg.SessionID, pasteBytes(inputText))
	if err == nil {
		select {
		case <-ctx.Done():
			err = ctx.Err()
		case <-time.After(b.cfg.EnterDelay):
			err = b.cfg.Input.SendInput(b.cfg.SessionID, keyEnter)
		}
	}
	if err != nil {
		b.mu.Lock()
		if b.pending == p {
			b.pending = nil
		}
		r.fail(err)
		b.mu.Unlock()
		return nil, fmt.Errorf("claudepty: send input: %w", err)
	}
	return r.stream, nil
}

func (b *Backend) newRun(id string) *run {
	r := &run{
		dec: claudejson.New().NewDecoder(chatstream.DecodeOptions{RunID: id, Provider: "claude", Model: b.cfg.Model, Now: b.cfg.Now}),
		fin: b.cfg.NewFinalizer(),
	}
	r.stream = chatbackend.NewStream(id, func() { b.cancel(r) })
	r.feed(initFrame)
	return r
}

// initFrame opens the run (claudejson turns system/init into run.start).
var initFrame = []byte(`{"type":"system","subtype":"init"}`)

func (r *run) feed(frame []byte) {
	evs, err := r.dec.Decode(chatstream.Frame{Data: frame})
	if err != nil {
		return
	}
	r.emit(evs)
}

func (r *run) emit(evs []chatstream.Event) {
	for _, ev := range evs {
		r.stream.Emit(r.fin.Push(ev)...)
	}
}

// fail ends the run with run.error (the decoder closes what is open first).
func (r *run) fail(cause error) { r.emit(r.dec.Close(cause)) }

// abort ends the run with run.abort.
func (r *run) abort() {
	r.feed([]byte(`{"type":"result","subtype":"success","terminal_reason":"aborted_streaming"}`))
}

// errorf ends the run with run.error carrying code (the decoder is closed first, so no part stays open).
func (r *run) errorf(now time.Time, code, format string, args ...any) {
	for _, ev := range r.dec.Close(nil) {
		if ev.IsTerminal() {
			ev.Code, ev.Message, ev.Retryable = code, fmt.Sprintf(format, args...), false
		}
		r.stream.Emit(r.fin.Push(ev)...)
	}
	if !r.stream.Done() {
		ev := chatstream.Event{V: chatstream.SchemaVersion, Verb: chatstream.VerbRunError, Time: now, Code: code, Message: fmt.Sprintf(format, args...)}
		r.stream.Emit(ev)
	}
}

// cancel is Run.Cancel.
func (b *Backend) cancel(r *run) {
	b.mu.Lock()
	now := b.cfg.Now()
	switch {
	case b.pending != nil && b.pending.run == r:
		if !b.pending.canceled {
			// The text may already be in the TUI: wait CancelGrace for it to start, and interrupt it if it does.
			b.pending.canceled = true
			b.pending.deadline = now.Add(b.cfg.CancelGrace)
		}
		b.mu.Unlock()
	case b.open != nil && b.open.run == r:
		send := b.open.cancelAt.IsZero()
		if send {
			b.open.cancelAt = now
		}
		b.mu.Unlock()
		if send {
			b.pressEsc()
		}
	default:
		b.mu.Unlock()
	}
}

func (b *Backend) pressEsc() {
	if b.cfg.Input != nil {
		_ = b.cfg.Input.SendInput(b.cfg.SessionID, keyEsc) // a failed Esc ends in CancelGrace's forced abort
	}
}

// onLine consumes one transcript line.
func (b *Backend) onLine(line []byte) {
	b.mu.Lock()
	esc := b.apply(b.mapper.Push(line))
	b.mu.Unlock()
	if esc {
		b.pressEsc()
	}
}

// publicText only replaces exact prompts this backend submitted with context.
// b.mu is held; ordinary terminal input is preserved verbatim.
func (b *Backend) publicText(text string) string {
	if public, ok := b.publicPrompts[promptKey(text)]; ok {
		return public
	}
	return text
}

func promptKey(text string) string { return strings.Join(strings.Fields(cleanText(text)), " ") }

// apply acts on mapper signals; b.mu is held. It reports whether Esc must be pressed (a canceled submission started).
func (b *Backend) apply(sigs []claudetranscript.Signal) (esc bool) {
	now := b.cfg.Now()
	for _, s := range sigs {
		switch s.Kind {
		case claudetranscript.TurnStart:
			t := &turn{promptID: s.PromptID}
			if p := b.pending; p != nil && sameInput(p.text, s.Prompt.Text) {
				t.run, b.pending = p.run, nil
				if p.canceled {
					t.cancelAt, esc = now, true
				}
			} else {
				t.run = b.newRun(b.cfg.NewRunID())
				b.publishObserved(chatbackend.ObservedRun{Run: t.run.stream, Text: b.publicText(s.Prompt.Text), Source: s.Prompt.Source, Origin: s.Prompt.Origin, TurnRef: s.PromptID})
			}
			if b.hookOpen != s.PromptID {
				b.hookOpen = "" // a hook turn that never saw its Stop is over once another prompt starts
			}
			if at, ok := b.stopped[s.PromptID]; ok {
				t.stopAt = at
				delete(b.stopped, s.PromptID)
			}
			b.open = t
		case claudetranscript.TurnFrame:
			if b.open != nil && b.open.promptID == s.PromptID {
				b.open.run.feed(s.Frame)
			}
		case claudetranscript.TurnEnd:
			if b.open != nil && b.open.promptID == s.PromptID {
				b.open.run.feed(s.Frame)
				b.open = nil
			}
			if b.hookOpen == s.PromptID {
				b.hookOpen = ""
			}
		case claudetranscript.TurnQueued:
			if b.open != nil {
				text := b.publicText(s.Text)
				ev := chatstream.Event{V: chatstream.SchemaVersion, Verb: chatstream.VerbActivity, Time: now, Value: mustJSON(map[string]string{"text": text})}
				// Set apart from the literal: a chatstream activity kind is not a Tether bus event kind, and the
				// events package's admission-policy scan reads Kind keys in any *Event literal.
				ev.Kind = ActivityQueuedInput
				b.open.run.stream.Emit(ev)
			}
			if p := b.pending; p != nil && sameInput(p.text, s.Text) {
				b.pending = nil
				p.run.errorf(now, CodeInputMerged, "the agent took this input inside the turn that was running (prompt %s)", s.PromptID)
			}
		}
	}
	return esc
}

// onHook consumes one hook signal.
func (b *Backend) onHook(h HookSignal) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.cfg.Now()
	switch h.Kind {
	case HookSessionStarted:
		if h.ClaudeSessionID != "" && b.claudeSID != "" && h.ClaudeSessionID != b.claudeSID {
			// A new transcript (/clear, a new session in the same TUI): the old one's open turn will not end there.
			b.apply(b.mapper.End(claudetranscript.EndForced))
			b.mapper = claudetranscript.Mapper{}
			b.hookOpen = ""
		}
		if h.ClaudeSessionID != "" {
			b.claudeSID = h.ClaudeSessionID
		}
	case HookTurnStarted:
		if h.ClaudeSessionID != "" && b.claudeSID == "" {
			b.claudeSID = h.ClaudeSessionID
		}
		if b.open == nil || b.open.promptID != h.PromptID {
			b.hookOpen = h.PromptID
		}
	case HookTurnCompleted:
		if b.hookOpen == h.PromptID || h.PromptID == "" {
			b.hookOpen = ""
		}
		if b.open != nil && (b.open.promptID == h.PromptID || h.PromptID == "") {
			if b.open.stopAt.IsZero() {
				b.open.stopAt = now
			}
		} else if h.PromptID != "" && b.mapper.Open() != h.PromptID {
			// The prompt line has not reached the transcript yet: remember, so its turn gets the grace timer too.
			if len(b.stopped) > 16 {
				b.stopped = map[string]time.Time{}
			}
			b.stopped[h.PromptID] = now
		}
	case HookSessionEnded:
		b.hookOpen = ""
		if b.open != nil {
			switch {
			case !b.open.cancelAt.IsZero():
				b.apply(b.mapper.End(claudetranscript.EndForcedAbort)) // it was being canceled: that is how it ended
			case !b.open.stopAt.IsZero():
				b.apply(b.mapper.End(claudetranscript.EndForced))
			default:
				r := b.open.run
				for _, sig := range b.mapper.End(claudetranscript.EndForced) {
					if sig.Kind == claudetranscript.TurnFrame { // keep what the transcript had; the end is an error
						r.feed(sig.Frame)
					}
				}
				b.open = nil
				r.errorf(now, CodeSessionEnded, "the Claude session ended during the turn")
			}
		}
		if p := b.pending; p != nil {
			b.pending = nil
			if p.canceled {
				p.run.abort()
			} else {
				p.run.errorf(now, CodeSessionEnded, "the Claude session ended before the input was taken")
			}
		}
	}
}

// tick enforces the grace timers.
func (b *Backend) tick(now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if p := b.pending; p != nil && !now.Before(p.deadline) {
		b.pending = nil
		if p.canceled {
			p.run.abort()
		} else {
			p.run.errorf(now, CodeInputNotSeen, "no turn with the submitted text started within %s", b.cfg.ClaimTimeout)
		}
	}
	if t := b.open; t != nil {
		switch {
		case !t.cancelAt.IsZero() && !now.Before(t.cancelAt.Add(b.cfg.CancelGrace)):
			b.apply(b.mapper.End(claudetranscript.EndForcedAbort))
		case !t.stopAt.IsZero() && !now.Before(t.stopAt.Add(b.cfg.FlushGrace)):
			b.apply(b.mapper.End(claudetranscript.EndForced))
		}
	}
}

// shutdown ends every open run; nothing is submitted afterwards.
func (b *Backend) shutdown(cause error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	if cause == nil {
		cause = chatbackend.ErrClosed
	}
	if b.open != nil {
		b.open.run.fail(cause)
		b.open = nil
	}
	if b.pending != nil {
		b.pending.run.fail(cause)
		b.pending = nil
	}
}

// publishObserved queues an observed run for Observed; b.mu is held. The queue is unbounded so the transcript reader
// never waits on the consumer.
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

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
