package terminalattach

import (
	"context"
	"errors"
	"io"

	"github.com/hollis-labs/libs/agent/sdk/chathost"
	"github.com/hollis-labs/libs/agent/sdk/session"
	"github.com/hollis-labs/libs/harness/core/adapters/agentsessions"
	"github.com/hollis-labs/libs/mesh/core/runtimecontract"
)

// TargetResolver maps an explicit UI target to the current host-issued binding
// and exact live PTY session. It must authenticate the caller independently of
// the target string; neither a conversation nor session ID is a credential.
// It must also resolve the conversation ID. It must not call back into this
// SDK Host: input/resize invoke it while the host's lease lock is held.
type TargetResolver func(context.Context, string) (Binding, error)

// PTY is the daemon's synchronous session I/O boundary. Its input and resize
// methods must finish delivery before returning; queued transports need their
// own lease check when the queued operation is actually delivered.
type PTY interface {
	AttachSessionWithSnapshot(context.Context, string, io.Writer, int64, func(agentsessions.AttachSnapshot) error) error
	SendInput(string, []byte) error
	ResizeSession(string, uint16, uint16) error
}

// ChatHost bridges terminal views to CHAT-0's durable conversation host. The
// SDK host owns attachment IDs and writer leases; this adapter never creates
// independent authority for input or resize.
type ChatHost struct {
	Host          *chathost.Host
	ResolveTarget TargetResolver
	PTY           PTY
}

var _ Host = (*ChatHost)(nil)

func (h *ChatHost) Resolve(ctx context.Context, target string) (Binding, error) {
	if h == nil || h.Host == nil || h.PTY == nil || h.ResolveTarget == nil {
		return Binding{}, ErrNotFound
	}
	b, err := h.ResolveTarget(ctx, target)
	if err != nil {
		return Binding{}, err
	}
	if !b.Terminal {
		return b, nil
	}
	record, err := h.Host.Load(ctx, session.Scope{OwnerID: runtimecontract.OwnerID(b.OwnerID), OperativeID: runtimecontract.OperativeID(b.OperativeID), ConversationID: runtimecontract.ConversationID(b.ConversationID)})
	if err != nil {
		return Binding{}, mapHostError(err)
	}
	if record.Phase != session.Active || record.Binding != sdkBinding(b) {
		return Binding{}, ErrStaleGeneration
	}
	return b, nil
}

func (h *ChatHost) Attach(ctx context.Context, b Binding, role Role) (string, error) {
	a, err := h.Host.Attach(ctx, sdkBinding(b), chathost.Role(role))
	return a.ID, mapHostError(err)
}

func (h *ChatHost) Detach(ctx context.Context, b Binding, id string, role Role) error {
	return mapHostError(h.Host.Detach(ctx, chathost.Attachment{ID: id, Binding: sdkBinding(b), Role: chathost.Role(role)}))
}

func (h *ChatHost) Claim(ctx context.Context, b Binding, id string, takeover bool) (Lease, error) {
	var l session.Lease
	var err error
	if takeover {
		l, err = h.Host.Takeover(ctx, sdkBinding(b), session.WriterID(id))
	} else {
		l, err = h.Host.Acquire(ctx, sdkBinding(b), session.WriterID(id))
	}
	if err != nil {
		return Lease{}, mapHostError(err)
	}
	return terminalLease(l, b), nil
}

func (h *ChatHost) Renew(ctx context.Context, l Lease) (Lease, error) {
	refreshed, err := h.Host.Renew(ctx, sdkLease(l))
	if err != nil {
		return Lease{}, mapHostError(err)
	}
	return terminalLease(refreshed, l.Binding), nil
}

func (h *ChatHost) Observe(ctx context.Context, b Binding, w io.Writer, since int64, onWindow func(Window) error) error {
	current, err := h.Resolve(ctx, b.ConversationID)
	if err != nil {
		return err
	}
	if current != b {
		return ErrStaleGeneration
	}
	return h.PTY.AttachSessionWithSnapshot(ctx, b.SessionID, &fencedOutput{ctx: ctx, host: h, binding: b, writer: w}, since, func(s agentsessions.AttachSnapshot) error {
		current, err := h.Resolve(ctx, b.ConversationID)
		if err != nil {
			return err
		}
		if current != b {
			return ErrStaleGeneration
		}
		return onWindow(Window{OldestOffset: s.OldestOffset, NextOffset: s.NextOffset})
	})
}

type fencedOutput struct {
	ctx     context.Context
	host    *ChatHost
	binding Binding
	writer  io.Writer
}

func (f *fencedOutput) Write(p []byte) (int, error) {
	b, err := f.host.Resolve(f.ctx, f.binding.ConversationID)
	if err != nil {
		return 0, err
	}
	if b != f.binding {
		return 0, ErrStaleGeneration
	}
	return f.writer.Write(p)
}

func (h *ChatHost) Input(ctx context.Context, l Lease, data []byte) error {
	return mapHostError(h.Host.WithLease(ctx, sdkLease(l), func() error {
		b, err := h.ResolveTarget(ctx, l.Binding.ConversationID)
		if err != nil {
			return err
		}
		if b != l.Binding {
			return ErrStaleGeneration
		}
		return h.PTY.SendInput(l.Binding.SessionID, data)
	}))
}

func (h *ChatHost) Resize(ctx context.Context, l Lease, rows, cols uint16) error {
	return mapHostError(h.Host.WithLease(ctx, sdkLease(l), func() error {
		b, err := h.ResolveTarget(ctx, l.Binding.ConversationID)
		if err != nil {
			return err
		}
		if b != l.Binding {
			return ErrStaleGeneration
		}
		return h.PTY.ResizeSession(l.Binding.SessionID, rows, cols)
	}))
}

func sdkBinding(b Binding) runtimecontract.Binding {
	return runtimecontract.Binding{OwnerID: runtimecontract.OwnerID(b.OwnerID), OperativeID: runtimecontract.OperativeID(b.OperativeID), SeatID: runtimecontract.SeatID(b.SeatID), ConversationID: runtimecontract.ConversationID(b.ConversationID), AttemptID: runtimecontract.AttemptID(b.AttemptID), Generation: runtimecontract.Generation(b.Generation)}
}
func sdkLease(l Lease) session.Lease {
	return session.Lease{ID: session.LeaseID(l.ID), Binding: sdkBinding(l.Binding), Writer: session.WriterID(l.WriterID), ExpiresAt: l.ExpiresAt}
}
func terminalLease(l session.Lease, b Binding) Lease {
	return Lease{ID: string(l.ID), Binding: b, WriterID: string(l.Writer), ExpiresAt: l.ExpiresAt}
}
func mapHostError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, session.ErrNotFound):
		return ErrNotFound
	case errors.Is(err, session.ErrStale):
		return ErrStaleGeneration
	case errors.Is(err, session.ErrLease):
		return ErrWriterHeld
	default:
		return err
	}
}
