// Package terminalattach adapts a conversation host's authoritative writer
// lease to a real, host-owned PTY. A view observes by default; a controller
// still needs an explicit lease before it can type or resize.
package terminalattach

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"
)

var (
	ErrNotFound        = errors.New("terminal attachment not found")
	ErrNotTerminal     = errors.New("runtime has no terminal")
	ErrStaleGeneration = errors.New("runtime generation changed")
	ErrWriterHeld      = errors.New("terminal writer lease held by another attachment")
	ErrNotWriter       = errors.New("attachment does not hold the writer lease")
	ErrInvalidRole     = errors.New("invalid attachment role")
)

// Binding mirrors runtimecontract.Binding plus the exact PTY session target.
// The adapter must compare every identity field against the live binding at
// the same boundary as input/resize; a precheck alone can race replacement.
type Binding struct {
	OwnerID        string `json:"owner_id"`
	OperativeID    string `json:"operative_id"`
	SeatID         string `json:"seat_id,omitempty"`
	ConversationID string `json:"conversation_id"`
	AttemptID      string `json:"attempt_id"`
	SessionID      string `json:"session_id"`
	Generation     uint64 `json:"generation"`
	Terminal       bool   `json:"terminal"`
}

type Role string

const (
	Viewer         Role = "viewer"
	ControllerRole Role = "controller"
)

// Lease is issued by the conversation host. It is never minted locally by
// this adapter; Host.Input and Host.Resize must validate it under that host's
// binding lock before delivering an effect to the PTY.
type Lease struct {
	ID        string
	Binding   Binding
	WriterID  string
	ExpiresAt time.Time
}

type Window struct {
	OldestOffset int64
	NextOffset   int64
}

type Host interface {
	Resolve(context.Context, string) (Binding, error)
	Attach(context.Context, Binding, Role) (string, error)
	Detach(context.Context, Binding, string, Role) error
	Claim(context.Context, Binding, string, bool) (Lease, error)
	Renew(context.Context, Lease) (Lease, error)
	Observe(context.Context, Binding, io.Writer, int64, func(Window) error) error
	Input(context.Context, Lease, []byte) error
	Resize(context.Context, Lease, uint16, uint16) error
}

type Attachment struct {
	ID             string    `json:"id"`
	ConversationID string    `json:"conversation_id"`
	SessionID      string    `json:"session_id"`
	Generation     uint64    `json:"generation"`
	Role           Role      `json:"role"`
	WriterUntil    time.Time `json:"writer_until"`
	Diagnostic     bool      `json:"diagnostic"` // PTY bytes are not committed chat events
}

type state struct {
	attachment Attachment
	binding    Binding
	lease      Lease
	closed     context.Context
	cancel     context.CancelFunc
}

type Controller struct {
	mu          sync.Mutex
	host        Host
	attachments map[string]*state
}

func New(host Host) *Controller {
	return &Controller{host: host, attachments: make(map[string]*state)}
}

func (c *Controller) Open(ctx context.Context, target string, role Role) (Attachment, error) {
	if role == "" {
		role = Viewer
	}
	if role != Viewer && role != ControllerRole {
		return Attachment{}, ErrInvalidRole
	}
	b, err := c.host.Resolve(ctx, target)
	if err != nil {
		return Attachment{}, err
	}
	if !b.Terminal {
		return Attachment{}, ErrNotTerminal
	}
	if b.OwnerID == "" || b.OperativeID == "" || b.ConversationID == "" || b.AttemptID == "" || b.SessionID == "" || b.Generation == 0 {
		return Attachment{}, ErrStaleGeneration
	}
	id, err := c.host.Attach(ctx, b, role)
	if err != nil {
		return Attachment{}, err
	}
	if id == "" {
		return Attachment{}, ErrNotFound
	}
	a := Attachment{ID: id, ConversationID: b.ConversationID, SessionID: b.SessionID, Generation: b.Generation, Role: role, Diagnostic: true}
	closed, cancel := context.WithCancel(context.Background())
	c.mu.Lock()
	if _, exists := c.attachments[id]; exists {
		c.mu.Unlock()
		cancel()
		return Attachment{}, errors.New("duplicate attachment id")
	}
	c.attachments[id] = &state{attachment: a, binding: b, closed: closed, cancel: cancel}
	c.mu.Unlock()
	return a, nil
}

func (c *Controller) Acquire(ctx context.Context, id string, takeover bool) (Attachment, string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, b, err := c.current(ctx, id)
	if err != nil {
		return Attachment{}, "", err
	}
	if s.attachment.Role != ControllerRole {
		return Attachment{}, "", ErrNotWriter
	}
	lease, err := c.host.Claim(ctx, b, id, takeover)
	if err != nil {
		return Attachment{}, "", err
	}
	if lease.ID == "" || lease.Binding != b || lease.WriterID != id {
		return Attachment{}, "", ErrStaleGeneration
	}
	s.lease = lease
	s.attachment.WriterUntil = lease.ExpiresAt
	return s.attachment, lease.ID, nil
}

func (c *Controller) Renew(ctx context.Context, id, token string) (Attachment, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, _, err := c.current(ctx, id)
	if err != nil {
		return Attachment{}, err
	}
	if !hasLease(s, token) {
		return Attachment{}, ErrNotWriter
	}
	lease, err := c.host.Renew(ctx, s.lease)
	if err != nil {
		return Attachment{}, err
	}
	s.lease = lease
	s.attachment.WriterUntil = lease.ExpiresAt
	return s.attachment, nil
}

func (c *Controller) Input(ctx context.Context, id, token string, data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, _, err := c.current(ctx, id)
	if err != nil {
		return err
	}
	if !hasLease(s, token) {
		return ErrNotWriter
	}
	return c.host.Input(ctx, s.lease, data)
}

func (c *Controller) Resize(ctx context.Context, id, token string, rows, cols uint16) error {
	if rows == 0 || cols == 0 {
		return errors.New("rows and cols must be > 0")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	s, _, err := c.current(ctx, id)
	if err != nil {
		return err
	}
	if !hasLease(s, token) {
		return ErrNotWriter
	}
	return c.host.Resize(ctx, s.lease, rows, cols)
}

func (c *Controller) Observe(ctx context.Context, id string, w io.Writer, since int64, onWindow func(Window, bool) error) error {
	if since < 0 {
		return errors.New("since must be non-negative")
	}
	c.mu.Lock()
	s, b, err := c.current(ctx, id)
	c.mu.Unlock()
	if err != nil {
		return err
	}
	streamCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.closed, cancel)
	defer stop()
	defer cancel()
	return c.host.Observe(streamCtx, b, w, since, func(window Window) error { return onWindow(window, since < window.OldestOffset) })
}

func (c *Controller) Close(ctx context.Context, id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.attachments[id]
	if s == nil {
		return nil
	}
	if err := c.host.Detach(ctx, s.binding, id, s.attachment.Role); err != nil && !errors.Is(err, ErrStaleGeneration) && !errors.Is(err, ErrNotFound) {
		return err
	}
	s.cancel()
	delete(c.attachments, id)
	return nil
}

func (c *Controller) current(ctx context.Context, id string) (*state, Binding, error) {
	s := c.attachments[id]
	if s == nil {
		return nil, Binding{}, ErrNotFound
	}
	b, err := c.host.Resolve(ctx, s.binding.ConversationID)
	if err != nil {
		return nil, Binding{}, err
	}
	if b != s.binding {
		return nil, Binding{}, ErrStaleGeneration
	}
	return s, b, nil
}

func hasLease(s *state, token string) bool {
	return token != "" && s.lease.ID == token && s.lease.WriterID == s.attachment.ID && s.lease.Binding == s.binding
}
