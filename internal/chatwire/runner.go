package chatwire

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	chat "github.com/hollis-labs/libs/agent/chat"
	"github.com/hollis-labs/libs/ui/ui-go/chatstream"

	"github.com/hollis-labs/tether/internal/chatbackend"
	"github.com/hollis-labs/tether/internal/conversation"
)

// Runtime is the chat backend a Tether session needs.
type Runtime string

// The runtimes.
const (
	// RuntimeClaudePTY is the Claude Code TUI in a PTY: internal/chatbackend/claudepty. Whole messages, no deltas.
	RuntimeClaudePTY Runtime = "claude-pty"
	// RuntimeClaudeStdio is Claude Code in streaming-stdio mode: internal/chatbackend/claudestdio. Deltas.
	RuntimeClaudeStdio Runtime = "claude-stdio"
	// RuntimeCodexAppServer is the Codex app-server (JSON-RPC over stdio). Not served yet: the decoder is libs PR #154,
	// not tagged. TODO(CW-20261118-0009): add the codex app-server backend and route this runtime to it.
	RuntimeCodexAppServer Runtime = "codex-app-server"
)

// Deltas reports whether runs of this runtime carry part.delta.
func (r Runtime) Deltas() bool { return r == RuntimeClaudeStdio }

// ErrUnsupportedRuntime is what the runner answers for a session whose runtime has no chat backend.
var ErrUnsupportedRuntime = errors.New("chatwire: the session's runtime has no chat backend")

// UnsupportedRuntimeError names the session and why it cannot be served. It matches ErrUnsupportedRuntime.
type UnsupportedRuntimeError struct {
	SessionID string
	Runtime   Runtime
	Reason    string
}

func (e *UnsupportedRuntimeError) Error() string {
	return fmt.Sprintf("chatwire: session %s (%s) has no chat backend: %s", e.SessionID, e.Runtime, e.Reason)
}

// Is matches ErrUnsupportedRuntime.
func (e *UnsupportedRuntimeError) Is(target error) bool { return target == ErrUnsupportedRuntime }

// ErrNoApprovals is Run.Answer's answer: Tether runs keep the agent's own permission handling (Info.Approvals false) and
// never raise approval.request.
var ErrNoApprovals = errors.New("chatwire: this runner raises no approvals")

// Backend is a chatbackend.Backend the runner runs for as long as it serves the session.
type Backend interface {
	chatbackend.Backend
	Run(ctx context.Context) error
}

// Sessions reaches Tether sessions. *app.Service implements it (internal/app/chat_wire.go).
type Sessions interface {
	// ChatRuntime reports which backend a session needs. A runtime with no chat backend at all (codex exec, an API
	// provider) is an *UnsupportedRuntimeError.
	ChatRuntime(sessionID string) (Runtime, error)
	// ChatBackend returns a new backend for a running session, not yet running.
	ChatBackend(sessionID string, rt Runtime) (Backend, error)
}

// Mapping ties chat conversations to Tether conversations. The Control implements it from the chat database alone: the
// runner never reaches chathost.
type Mapping interface {
	TetherConversation(ctx context.Context, conversationID string) (string, error)
	CurrentFor(ctx context.Context, tetherConversationID string) (string, error)
}

// Observer is where runs the agent started are published: chat.Service.Observe.
type Observer interface {
	Observe(ctx context.Context, conversationID string, turn chat.Turn, run chat.Run) error
}

// RunnerConfig configures a Runner.
type RunnerConfig struct {
	Mapping Mapping
	// Target resolves a Tether conversation to its current session and attempt (conversation.Resolver.ByConversation).
	Target   func(ctx context.Context, tetherConversationID string) (conversation.Target, error)
	Sessions Sessions
	Logger   *slog.Logger
}

// Runner is the chat.Runner for agents Tether runs (CW-20261118-0011). It resolves a conversation's current session
// through internal/conversation, runs one backend per session (picked by runtime), submits turns to it, and publishes
// the turns the agent ran on its own (typed into the terminal, a mail wake) through Observer. Its side is replaced when
// the conversation moves to another session; Moved (from the relaunch path) signals that on Replaced.
type Runner struct {
	cfg      RunnerConfig
	ctx      context.Context
	stop     context.CancelFunc
	wg       sync.WaitGroup
	observer atomic.Pointer[Observer]
	replaced chan string

	mu    sync.Mutex
	bound map[string]*binding // by Tether conversation
}

// binding is the backend serving one Tether conversation's current session.
type binding struct {
	target  conversation.Target
	runtime Runtime
	backend Backend
	cancel  context.CancelFunc
}

var _ chat.Runner = (*Runner)(nil)

// NewRunner returns a Runner. Call SetObserver once the Service exists, and Close when done.
func NewRunner(cfg RunnerConfig) (*Runner, error) {
	if cfg.Mapping == nil || cfg.Target == nil || cfg.Sessions == nil {
		return nil, errors.New("chatwire: RunnerConfig needs Mapping, Target and Sessions")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	ctx, stop := context.WithCancel(context.Background())
	return &Runner{cfg: cfg, ctx: ctx, stop: stop, replaced: make(chan string, 64), bound: map[string]*binding{}}, nil
}

// SetObserver sets where observed runs go. Until it is set they are read and dropped.
func (r *Runner) SetObserver(o Observer) { r.observer.Store(&o) }

// Close stops every backend and waits for them.
func (r *Runner) Close() {
	r.stop()
	r.wg.Wait()
}

// RunnerRef is the runner's reference for a target: its session and attempt.
func RunnerRef(t conversation.Target) string { return t.SessionID + "/" + t.AttemptID }

func (r *Runner) target(ctx context.Context, conversationID string) (conversation.Target, error) {
	tether, err := r.cfg.Mapping.TetherConversation(ctx, conversationID)
	if err != nil {
		return conversation.Target{}, err
	}
	return r.cfg.Target(ctx, tether)
}

// bind returns the backend for a target's session, starting one when the conversation has none or has moved since.
func (r *Runner) bind(t conversation.Target) (*binding, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ctx.Err() != nil {
		return nil, chat.ErrServiceClosed
	}
	if b := r.bound[t.ConversationID]; b != nil {
		if b.target.SessionID == t.SessionID && b.target.AttemptID == t.AttemptID {
			return b, nil
		}
		b.cancel()
		delete(r.bound, t.ConversationID)
	}
	rt, err := r.cfg.Sessions.ChatRuntime(t.SessionID)
	if err != nil {
		return nil, err
	}
	if rt == RuntimeCodexAppServer {
		// TODO(CW-20261118-0009): the codex app-server decoder (libs PR #154) is not tagged; serve this runtime once a
		// chatbackend for it exists.
		return nil, &UnsupportedRuntimeError{SessionID: t.SessionID, Runtime: rt, Reason: "the codex app-server backend is CW-20261118-0009"}
	}
	be, err := r.cfg.Sessions.ChatBackend(t.SessionID, rt)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(r.ctx)
	b := &binding{target: t, runtime: rt, backend: be, cancel: cancel}
	r.bound[t.ConversationID] = b
	r.wg.Add(2)
	go func() {
		defer r.wg.Done()
		if err := be.Run(ctx); err != nil && ctx.Err() == nil {
			r.cfg.Logger.Warn("chatwire: backend stopped", "session", t.SessionID, "error", err)
		}
		cancel()
		r.mu.Lock()
		if r.bound[t.ConversationID] == b {
			delete(r.bound, t.ConversationID)
		}
		r.mu.Unlock()
	}()
	go func() {
		defer r.wg.Done()
		r.forward(ctx, t.ConversationID, be)
	}()
	return b, nil
}

// wait returns once the backend follows its session (or after readyTimeout), so a turn submitted next is seen.
func (b *binding) wait(ctx context.Context) {
	rb, ok := b.backend.(readier)
	if !ok {
		return
	}
	select {
	case <-rb.Ready():
	case <-ctx.Done():
	case <-time.After(readyTimeout):
	}
}

// readier is a backend that reports when Run has begun following its session (claudestdio: subscribed to the tap).
// Output written before then would reach no run.
type readier interface{ Ready() <-chan struct{} }

const readyTimeout = 5 * time.Second

// forward publishes the backend's observed runs on the Tether conversation's current chat conversation.
func (r *Runner) forward(ctx context.Context, tether string, be Backend) {
	for o := range be.Observed() {
		chatID, err := r.cfg.Mapping.CurrentFor(ctx, tether)
		op := r.observer.Load()
		if err != nil || op == nil {
			go drainRun(o.Run)
			continue
		}
		meta, _ := json.Marshal(map[string]string{"input_source": o.Source, "input_origin": o.Origin, "turn_ref": o.TurnRef}) //nolint:errchkjson // strings only
		if err := (*op).Observe(ctx, chatID, chat.Turn{Text: o.Text, Meta: meta}, run{o.Run}); err != nil {
			if ctx.Err() == nil {
				r.cfg.Logger.Warn("chatwire: observed run not published", "conversation", chatID, "error", err)
			}
			go drainRun(o.Run)
		}
	}
}

func drainRun(run chatbackend.Run) {
	for range run.Events() { //nolint:revive // the events are discarded on purpose
	}
}

// Start implements chat.Runner.
func (r *Runner) Start(ctx context.Context, turn chat.Turn) (chat.Run, error) {
	t, err := r.target(ctx, turn.ConversationID)
	if err != nil {
		return nil, err
	}
	b, err := r.bind(t)
	if err != nil {
		return nil, err
	}
	b.wait(ctx)
	out, err := b.backend.Submit(ctx, chatbackend.Turn{RunID: turn.RunID, Text: turn.Text})
	if err != nil {
		return nil, err
	}
	return run{out}, nil
}

// Info implements chat.Runner. It also starts the conversation's backend, so turns the agent runs on its own reach the
// stream before anyone submits.
func (r *Runner) Info(ctx context.Context, conversationID string) (chat.Info, error) {
	t, err := r.target(ctx, conversationID)
	if err != nil {
		return chat.Info{}, err
	}
	rt, err := r.cfg.Sessions.ChatRuntime(t.SessionID)
	if err != nil {
		return chat.Info{}, err
	}
	if _, err := r.bind(t); err != nil && !errors.Is(err, ErrUnsupportedRuntime) {
		r.cfg.Logger.Warn("chatwire: backend not started", "conversation", conversationID, "session", t.SessionID, "error", err)
	}
	return chat.Info{Deltas: rt.Deltas(), Approvals: false, RunnerRef: RunnerRef(t)}, nil
}

// Record implements chat.Runner. A host answer is not written into the agent's session: the agent keeps its own
// history, and Tether has no hook that answers for it today.
func (r *Runner) Record(context.Context, chat.Turn, string) error { return nil }

// Replaced implements chat.Runner.
func (r *Runner) Replaced() <-chan string { return r.replaced }

// Moved is the relaunch path's signal (after its transaction committed): the conversation's Tether side moved to
// another session. The old session's backend stops, the new one starts if its session is up, and conversationID is
// sent on Replaced so the Service re-reads the generation.
func (r *Runner) Moved(ctx context.Context, conversationID string) {
	if t, err := r.target(ctx, conversationID); err == nil {
		if _, berr := r.bind(t); berr != nil && !errors.Is(berr, ErrUnsupportedRuntime) {
			// The new session is often not running yet; the next Start or Info binds it.
			r.cfg.Logger.Debug("chatwire: new session not bound yet", "conversation", conversationID, "error", berr)
		}
	}
	select {
	case r.replaced <- conversationID:
	case <-r.ctx.Done():
	case <-ctx.Done():
	}
}

// run adapts a chatbackend.Run to chat.Run.
type run struct{ chatbackend.Run }

func (x run) Events() iter.Seq2[chatstream.Event, error] { return x.Run.Events() }
func (x run) Cancel()                                    { x.Run.Cancel() }
func (run) Answer(context.Context, string, chat.Decision) error {
	return ErrNoApprovals
}
