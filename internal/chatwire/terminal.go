package chatwire

import (
	"context"
	"errors"
	"fmt"

	chat "github.com/hollis-labs/libs/agent/chat"
	"github.com/hollis-labs/libs/agent/sdk/chathost"
	"github.com/hollis-labs/libs/mesh/core/runtimecontract"

	"github.com/hollis-labs/tether/internal/conversation"
	"github.com/hollis-labs/tether/internal/terminalattach"
)

// TerminalSessions is the daemon's PTY boundary for terminal attachments (*app.Service).
type TerminalSessions interface {
	terminalattach.PTY
	// HasTerminal reports whether this daemon runs a live PTY for the session. It is called under the chat host's
	// lease lock, so it must read only in-process state.
	HasTerminal(sessionID string) bool
}

// TerminalConfig configures NewTerminalHost.
type TerminalConfig struct {
	// Host is the chat host the chat wire's Control uses (OpenHost): one writer lease for chat and terminal attach.
	Host *chathost.Host
	// Store is the Tether store the targets resolve through (conversation.Resolver).
	Store conversation.Store
	// Sessions delivers PTY input, resize and output.
	Sessions TerminalSessions
	// Authorize decides whether the caller may reach a conversation of owner. Defaults to the chat wire's Authorize
	// for read; the route table has already required the terminal or input scope.
	Authorize func(ctx context.Context, owner string) error
}

// NewTerminalHost is the terminal attachment host (internal/api /terminal-attachments, CW-20261118-0012) over the
// chat host the chat wire's Control uses. A conversation's chat host binding is HostBinding of its Tether target, the
// same binding the Control derives for the seat's chat conversation, so a terminal controller and a chat controller
// contend for one writer lease: a takeover by either refuses the other's input.
//
// Targets resolve through conversation.Resolver, never through the Control or the chat host: the TargetResolver runs
// while the chat host's lease lock is held (terminalattach.ChatHost.Input and Resize).
func NewTerminalHost(cfg TerminalConfig) (terminalattach.Host, error) {
	if cfg.Host == nil || cfg.Store == nil || cfg.Sessions == nil {
		return nil, errors.New("chatwire: TerminalConfig needs Host, Store and Sessions")
	}
	if cfg.Authorize == nil {
		cfg.Authorize = func(ctx context.Context, owner string) error { return Authorize(ctx, owner, chat.PermRead) }
	}
	t := &TerminalTargets{Resolver: conversation.Resolver{Store: cfg.Store}, Sessions: cfg.Sessions, Authorize: cfg.Authorize}
	return &terminalHost{
		ChatHost: &terminalattach.ChatHost{Host: cfg.Host, ResolveTarget: t.Resolve, PTY: cfg.Sessions},
		host:     cfg.Host,
		targets:  t,
	}, nil
}

// TerminalTargets is the terminal attachments' TargetResolver: a conversation or session ID to the current chat host
// binding and exact PTY session. It reads the Tether store and the sessions' in-process PTY table only.
type TerminalTargets struct {
	Resolver  conversation.Resolver
	Sessions  interface{ HasTerminal(sessionID string) bool }
	Authorize func(ctx context.Context, owner string) error
}

// Resolve implements terminalattach.TargetResolver. A superseded session target is terminalattach.ErrStaleGeneration;
// a caller Authorize refuses gets terminalattach.ErrNotFound, as for a conversation that does not exist.
func (r *TerminalTargets) Resolve(ctx context.Context, target string) (terminalattach.Binding, error) {
	b, _, err := r.resolve(ctx, target)
	return b, err
}

// resolve returns the terminal binding and the chat host binding it carries.
func (r *TerminalTargets) resolve(ctx context.Context, target string) (terminalattach.Binding, runtimecontract.Binding, error) {
	t, err := r.Resolver.Resolve(ctx, target)
	switch {
	case errors.Is(err, conversation.ErrNotFound):
		return terminalattach.Binding{}, runtimecontract.Binding{}, terminalattach.ErrNotFound
	case errors.Is(err, conversation.ErrStale):
		return terminalattach.Binding{}, runtimecontract.Binding{}, terminalattach.ErrStaleGeneration
	case err != nil:
		return terminalattach.Binding{}, runtimecontract.Binding{}, err
	}
	if err := r.Authorize(ctx, t.Owner); err != nil {
		return terminalattach.Binding{}, runtimecontract.Binding{}, fmt.Errorf("%w: %w", terminalattach.ErrNotFound, err)
	}
	// The chat host binding as the chat wire derives it for the seat's chat conversation: the chat agent is the seat
	// (Wire.agent resolves agents with BySeat), its scope the seat, its owner the conversation's.
	b := HostBinding(t.Owner, t.Seat, t.Seat, t)
	return terminalattach.Binding{
		OwnerID:        string(b.OwnerID),
		OperativeID:    string(b.OperativeID),
		SeatID:         string(b.SeatID),
		ConversationID: string(b.ConversationID),
		AttemptID:      string(b.AttemptID),
		SessionID:      t.SessionID,
		Generation:     uint64(b.Generation),
		// A worker's PTY is on the worker; only this daemon's own PTYs are attachable here.
		Terminal: t.WorkerID == "" && t.SessionID != "" && r.Sessions.HasTerminal(t.SessionID),
	}, b, nil
}

// terminalHost is terminalattach.ChatHost plus bringing the chat host to the Tether conversation's generation before a
// target is resolved, so a conversation no chat client has opened, or one relaunched since, can be attached to.
// Resolve runs outside the lease lock (Controller.Open and before every lease call), so it may call the chat host.
type terminalHost struct {
	*terminalattach.ChatHost
	host    *chathost.Host
	targets *TerminalTargets
}

func (h *terminalHost) Resolve(ctx context.Context, target string) (terminalattach.Binding, error) {
	b, hb, err := h.targets.resolve(ctx, target)
	if err != nil || !b.Terminal || b.OperativeID == "" {
		return b, err // no seat: Controller.Open refuses the incomplete binding
	}
	if err := syncHost(ctx, h.host, hb); err != nil {
		return terminalattach.Binding{}, err
	}
	return h.ChatHost.Resolve(ctx, target)
}
