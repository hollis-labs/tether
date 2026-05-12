package acpadapter

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
)

// version is the agent's reported build version in the initialize result.
// Kept tiny on purpose; the host can override via WithAgentVersion.
const version = "0.1.0"

// Adapter is the top-level ACP server. It composes:
//
//   - A Dispatcher (bidirectional JSON-RPC engine over stdio)
//   - An AuthGate (token + scope enforcement mirroring `mux mcp`)
//   - A Service (the host integration seam)
//   - Per-connection session ownership tracking for clean shutdown
//
// Adapter is constructed once per `mux acp` subprocess invocation; the
// editor spawns one subprocess per session-graph it wants to drive.
type Adapter struct {
	svc       Service
	auth      *AuthGate
	agentName string
	logger    *slog.Logger

	// ownedSessions tracks SessionIDs created or resumed on this
	// connection so we can close them on EOF/teardown. Multi-client
	// is supported by the daemon side: if another ACP connection
	// resumes the same session, both will independently track it,
	// and either disconnecting only closes its own bookkeeping —
	// the underlying mux session stays alive until all references
	// drop. (Detailed reference counting lives host-side; the
	// adapter just tracks "this connection's responsibility".)
	ownedMu       sync.Mutex
	ownedSessions map[SessionID]struct{}
}

// Option configures the Adapter at construction time.
type Option func(*Adapter)

// WithAgentName overrides the agentInfo.name returned by initialize.
// Default is "mux".
func WithAgentName(name string) Option { return func(a *Adapter) { a.agentName = name } }

// WithLogger sets the slog logger used for adapter-internal warnings.
// Default is slog.Default(). The logger is wired to stderr by the
// `mux acp` subcommand so warn lines don't pollute the stdout protocol
// stream.
func WithLogger(l *slog.Logger) Option { return func(a *Adapter) { a.logger = l } }

// New constructs an Adapter wrapping svc with the given auth gate.
// Pass an empty token to disable authentication (development only).
func New(svc Service, token string, scopes []string, opts ...Option) *Adapter {
	a := &Adapter{
		svc:           svc,
		auth:          NewAuthGate(token, scopes),
		agentName:     "mux",
		logger:        slog.Default(),
		ownedSessions: map[SessionID]struct{}{},
	}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

// Run drives the ACP server over stdin/stdout (or any io.Reader/Writer
// pair), blocking until ctx is canceled or the peer closes stdin.
// Owned sessions are best-effort closed on shutdown.
func (a *Adapter) Run(ctx context.Context, stdin io.Reader, stdout io.Writer) error {
	r := NewReader(stdin)
	w := NewWriter(stdout)
	d := NewDispatcher(r, w)

	a.registerHandlers(d)

	err := d.Run(ctx)
	a.shutdownOwnedSessions(ctx)

	// io.EOF is the normal "editor disconnected" exit; surface as nil
	// so callers can distinguish clean shutdown from real errors.
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

func (a *Adapter) registerHandlers(d *Dispatcher) {
	// Inbound requests (client → agent).
	d.HandleMethod("initialize", a.handleInitialize)
	d.HandleMethod("authenticate", a.handleAuthenticate)
	d.HandleMethod("session/new", a.handleSessionNew(d))
	d.HandleMethod("session/prompt", a.handleSessionPrompt(d))
	d.HandleMethod("session/close", a.handleSessionClose)
	d.HandleMethod("session/resume", a.handleSessionResume)

	// Inbound notifications (client → agent).
	d.HandleNotification("session/cancel", a.handleSessionCancel)
}

// trackOwned adds id to the owned-sessions set. Idempotent.
func (a *Adapter) trackOwned(id SessionID) {
	a.ownedMu.Lock()
	defer a.ownedMu.Unlock()
	a.ownedSessions[id] = struct{}{}
}

// untrackOwned removes id from the owned-sessions set.
func (a *Adapter) untrackOwned(id SessionID) {
	a.ownedMu.Lock()
	defer a.ownedMu.Unlock()
	delete(a.ownedSessions, id)
}

// shutdownOwnedSessions best-effort closes every still-owned session
// on adapter teardown. Errors are logged but don't propagate — we're
// already in the shutdown path.
func (a *Adapter) shutdownOwnedSessions(ctx context.Context) {
	a.ownedMu.Lock()
	ids := make([]SessionID, 0, len(a.ownedSessions))
	for id := range a.ownedSessions {
		ids = append(ids, id)
	}
	a.ownedSessions = map[SessionID]struct{}{}
	a.ownedMu.Unlock()

	// Detach context so it doesn't share cancellation with the
	// dispatcher's already-canceled ctx; CloseSession needs a live
	// context to round-trip through the daemon.
	closeCtx := context.WithoutCancel(ctx)

	for _, id := range ids {
		if err := a.svc.CloseSession(closeCtx, id); err != nil {
			a.logger.Warn("acp: close on shutdown failed", "session_id", id, "err", err)
		}
	}
}
