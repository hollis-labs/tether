// Package opsstatus posts Tether session lifecycle events to the Ops status
// stream (G3, CW-20261121-0154; convention caster://ops/design.system-of-record-v2
// section 5). Tether is the system emitter for session_started, session_ended
// and session_lost on sessions bound to a Torque task or a workstream.
//
// The emitter follows the durable event log from a stored cursor, so a daemon
// restart or an Ops outage delays posts rather than losing them. Each post
// carries an idempotency key per (session, kind), so a replay is harmless.
package opsstatus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/store"
)

// ConsumerName keys this emitter's row in event_consumer_cursors.
const ConsumerName = "ops_status"

// Kinds and states per the convention (section 5.2 and 5.3).
const (
	KindSessionStarted = "session_started"
	KindSessionEnded   = "session_ended"
	KindSessionLost    = "session_lost"
)

// Post is one ops_status_post call.
type Post struct {
	State          string     `json:"state"`
	Kind           string     `json:"kind"`
	Summary        string     `json:"summary"`
	Body           string     `json:"body,omitempty"`
	WorkstreamID   string     `json:"workstream_id,omitempty"`
	Task           string     `json:"task,omitempty"`
	Session        string     `json:"session"`
	Evidence       []Evidence `json:"evidence,omitempty"`
	IdempotencyKey string     `json:"idempotency_key"`
	// RunID is sent as a header, not an argument.
	RunID string `json:"-"`
}

// Evidence is one typed reference on a post.
type Evidence struct {
	Kind string `json:"kind"`
	Ref  string `json:"ref"`
}

// Poster delivers a post. A *RetryableError means hold and try again; any
// other error means the post was refused for good and is skipped.
type Poster interface {
	Post(ctx context.Context, p Post) error
}

// RetryableError marks a failure that may succeed later (Ops down, not yet
// accepting Tether's posts, a token being rotated).
type RetryableError struct{ Err error }

func (e *RetryableError) Error() string { return e.Err.Error() }
func (e *RetryableError) Unwrap() error { return e.Err }

// Store is the slice of the Tether store the emitter reads and writes.
type Store interface {
	GetSession(id string) (*store.SessionRow, error)
	MaxEventSeqContext(ctx context.Context) (int64, error)
	EventConsumerCursor(ctx context.Context, consumer string) (int64, bool, error)
	AdvanceEventConsumerCursor(ctx context.Context, consumer string, seq int64) error
}

// Options tune the emitter. Zero values take the defaults.
type Options struct {
	// MaxAge is how long an undelivered event is retried before it is skipped.
	MaxAge time.Duration
	// BackoffFloor and BackoffCeiling bound the retry wait.
	BackoffFloor   time.Duration
	BackoffCeiling time.Duration
	// Host names this daemon's machine for sessions with no worker; default os.Hostname.
	Host string
	// Now is for tests.
	Now func() time.Time
}

// Emitter follows session.state_changed and posts lifecycle status.
type Emitter struct {
	store  Store
	bus    events.Bus
	poster Poster
	opts   Options

	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

// New builds an emitter. Start runs it.
func New(st Store, bus events.Bus, poster Poster, opts Options) *Emitter {
	if opts.MaxAge <= 0 {
		opts.MaxAge = 24 * time.Hour
	}
	if opts.BackoffFloor <= 0 {
		opts.BackoffFloor = 2 * time.Second
	}
	if opts.BackoffCeiling <= 0 {
		opts.BackoffCeiling = 5 * time.Minute
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Host == "" {
		opts.Host, _ = os.Hostname()
	}
	return &Emitter{store: st, bus: bus, poster: poster, opts: opts}
}

// Start resumes from the stored cursor. With no cursor yet it starts at the
// current head: history before the emitter existed is not posted.
func (e *Emitter) Start(parent context.Context) error {
	cursor, ok, err := e.store.EventConsumerCursor(parent, ConsumerName)
	if err != nil {
		return fmt.Errorf("opsstatus: read cursor: %w", err)
	}
	if !ok {
		if cursor, err = e.store.MaxEventSeqContext(parent); err != nil {
			return fmt.Errorf("opsstatus: read head: %w", err)
		}
		if err := e.store.AdvanceEventConsumerCursor(parent, ConsumerName, cursor); err != nil {
			return fmt.Errorf("opsstatus: seed cursor: %w", err)
		}
	}
	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
	e.cancel = cancel
	e.done = make(chan struct{})
	filter := events.Filter{Kinds: []string{events.KindSessionStateChanged}, SinceSeq: cursor}
	ready := make(chan struct{})
	go func() {
		defer close(e.done)
		defer cancel()
		events.Follow(ctx, e.bus, filter, events.FollowOptions{Name: "opsstatus", Ready: func() { close(ready) }}, func(ev events.Event) {
			e.handle(ctx, ev)
		})
	}()
	select {
	case <-ready:
	case <-parent.Done():
	}
	slog.Info("opsstatus: following session lifecycle", "since_seq", cursor)
	return nil
}

// Close stops the emitter and waits for it.
func (e *Emitter) Close() {
	if e == nil || e.cancel == nil {
		return
	}
	e.once.Do(e.cancel)
	<-e.done
}

type statePayload struct {
	From     string `json:"from"`
	To       string `json:"to"`
	ExitCode *int   `json:"exit_code,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

func (e *Emitter) handle(ctx context.Context, ev events.Event) {
	if post, ok := e.postFor(ev); ok {
		e.deliver(ctx, ev, post)
	}
	if ctx.Err() != nil {
		return
	}
	if err := e.store.AdvanceEventConsumerCursor(ctx, ConsumerName, ev.Seq); err != nil {
		slog.Warn("opsstatus: advance cursor", "seq", ev.Seq, "err", err)
	}
}

// deliver retries a retryable failure until it succeeds, the event ages out,
// or the emitter stops. The event stream waits behind it, so posts stay in order.
func (e *Emitter) deliver(ctx context.Context, ev events.Event, p Post) {
	backoff := e.opts.BackoffFloor
	for attempt := 1; ctx.Err() == nil; attempt++ {
		err := e.poster.Post(ctx, p)
		if err == nil {
			slog.Info("opsstatus: posted", "kind", p.Kind, "session", p.Session, "task", p.Task, "workstream", p.WorkstreamID, "seq", ev.Seq)
			return
		}
		var retry *RetryableError
		if !errors.As(err, &retry) {
			slog.Warn("opsstatus: post refused; skipped", "kind", p.Kind, "session", p.Session, "seq", ev.Seq, "err", err)
			return
		}
		if age := e.opts.Now().Sub(ev.At); !ev.At.IsZero() && age > e.opts.MaxAge {
			slog.Warn("opsstatus: post undeliverable past max age; skipped", "kind", p.Kind, "session", p.Session, "seq", ev.Seq, "age", age, "err", err)
			return
		}
		if attempt == 1 || attempt%10 == 0 {
			slog.Warn("opsstatus: post failed; holding", "kind", p.Kind, "session", p.Session, "seq", ev.Seq, "attempt", attempt, "retry_in", backoff, "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > e.opts.BackoffCeiling {
			backoff = e.opts.BackoffCeiling
		}
	}
}

// postFor maps one session.state_changed event to a post, or reports none.
// Only sessions bound to a task or a workstream post: an ad-hoc session has no
// item to report on.
func (e *Emitter) postFor(ev events.Event) (Post, bool) {
	if ev.SessionID == "" {
		return Post{}, false
	}
	var sp statePayload
	if err := json.Unmarshal([]byte(ev.PayloadJSON), &sp); err != nil {
		return Post{}, false
	}
	kind, state, ok := Classify(sp.To, sp.Reason, sp.ExitCode)
	if !ok {
		return Post{}, false
	}
	row, err := e.store.GetSession(ev.SessionID)
	if err != nil || row == nil {
		return Post{}, false
	}
	task, ws := row.TaskID.String, row.WorkstreamID.String
	if task == "" && ws == "" {
		return Post{}, false
	}
	p := Post{
		State:          state,
		Kind:           kind,
		Summary:        summary(kind, row, sp, e.opts.Host),
		WorkstreamID:   ws,
		Task:           task,
		Session:        row.ID,
		Evidence:       []Evidence{{Kind: "tether.session", Ref: "tether://session/" + row.ID}},
		IdempotencyKey: "tether:" + row.ID + ":" + kind,
		RunID:          row.RunID.String,
	}
	if kind != KindSessionStarted {
		p.Body = truncate(endBody(sp), 4000)
	}
	p.Summary = truncate(p.Summary, 500)
	return p, true
}

// Classify maps a session state transition to a lifecycle kind and the
// stream's state enum. Recovery transitions (detached, orphaned) post nothing.
func Classify(to, reason string, exit *int) (kind, state string, ok bool) {
	switch to {
	case "running":
		return KindSessionStarted, "started", true
	case "completed":
		return KindSessionEnded, "note", true
	case "killed":
		if lostReason(reason) {
			return KindSessionLost, "note", true
		}
		return KindSessionEnded, "note", true
	case "failed":
		if exit != nil && *exit == 0 && !lostReason(reason) {
			return KindSessionEnded, "note", true
		}
		return KindSessionLost, "note", true
	}
	return "", "", false
}

// lostReason reports an end the agent did not choose and nobody asked for:
// a hub-ordered kill, shim recovery finding it gone, a daemon shutdown or
// startup sweep, or a launch that never started.
func lostReason(reason string) bool {
	r := strings.ToLower(reason)
	switch {
	case strings.HasPrefix(r, "hub_kill:"),
		r == "shim_recovery",
		r == "never_started",
		strings.Contains(r, "daemon-shutdown"),
		strings.Contains(r, "daemon_shutdown"),
		strings.Contains(r, "swept"):
		return true
	}
	return false
}

func summary(kind string, row *store.SessionRow, sp statePayload, host string) string {
	who := row.LogicalAgentID
	if who == "" {
		who = "agent"
	}
	what := "session " + shortID(row.ID)
	if row.TaskID.String != "" {
		what += " for " + row.TaskID.String
	}
	switch kind {
	case KindSessionStarted:
		s := who + " " + what + " started"
		if row.WorkerID != "" {
			s += " on " + row.WorkerID
		} else if host != "" {
			s += " on " + host
		}
		return s
	case KindSessionLost:
		return who + " " + what + " lost (" + endReason(sp) + ")"
	default:
		return who + " " + what + " ended (" + endReason(sp) + ")"
	}
}

func endReason(sp statePayload) string {
	parts := []string{sp.To}
	if sp.Reason != "" {
		parts = append(parts, sp.Reason)
	}
	if sp.ExitCode != nil {
		parts = append(parts, fmt.Sprintf("exit %d", *sp.ExitCode))
	}
	return strings.Join(parts, ", ")
}

func endBody(sp statePayload) string {
	return "from " + sp.From + " to " + endReason(sp)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
