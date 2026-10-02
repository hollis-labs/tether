package channels

import (
	"context"
	"fmt"

	gomsg "github.com/hollis-labs/go-messaging"
)

// ExistingMessage attaches a staged session output, preserving its body and ID.
// Actor is the daemon service performing routing; publisher remains the session.
type ExistingMessage struct {
	MessageID         string
	Channel           string
	SessionID         string
	Actor             gomsg.Address
	LaunchDisplayName string
}

// ExistingMessageBackend is optional: ordinary channel backends need not host
// staged session outputs. Authorization and atomic publication belong here.
type ExistingMessageBackend interface {
	AttachChannelMessage(context.Context, ExistingMessage) (gomsg.Envelope, error)
}

func (s *Service) AttachExisting(ctx context.Context, req ExistingMessage) (gomsg.Envelope, error) {
	if err := ValidateName(req.Channel); err != nil {
		return gomsg.Envelope{}, err
	}
	if req.MessageID == "" || req.SessionID == "" || req.Actor.IsZero() {
		return gomsg.Envelope{}, ErrInvalid
	}
	if _, err := gomsg.ParseURN(req.Actor.URN()); err != nil {
		return gomsg.Envelope{}, ErrInvalid
	}
	backend, ok := s.backend.(ExistingMessageBackend)
	if !ok {
		return gomsg.Envelope{}, fmt.Errorf("%w: backend cannot attach existing messages", ErrInvalid)
	}
	return backend.AttachChannelMessage(ctx, req)
}
