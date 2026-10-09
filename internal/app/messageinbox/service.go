// Package messageinbox owns an inbox pull and its optional recipient-session
// consumption. Transports supply decoded addresses and session observations.
package messageinbox

import (
	"context"
	"log"

	"github.com/hollis-labs/substrate/mesh/messaging"
)

// Store keeps atomic delivered marking in Inbox; Consume settles a delivery.
type Store interface {
	Inbox(context.Context, messaging.Address, messaging.Filter) ([]messaging.Envelope, error)
	Consume(context.Context, string, messaging.Address) error
}

// Sessions observes daemon-owned runtime and session records, not a caller's
// assertion that it owns a mailbox.
type Sessions interface {
	Live(string) bool
	LogicalAgentID(string) (string, error)
}

// Service owns the pull/consume operation over narrow storage/session seams.
type Service struct {
	store    Store
	sessions Sessions
}

// Result is the application-owned inbox response. Envelopes are the public
// go-messaging domain contract, not embedded or aliased storage rows.
type Result struct {
	Messages []messaging.Envelope `json:"messages"`
}

// New composes an inbox with its daemon session observations. Nil sessions
// support plain operator pulls without recipient-session consumption.
func New(store Store, sessions Sessions) *Service {
	return &Service{store: store, sessions: sessions}
}

// Pull preserves Inbox's result even if a subsequent Consume fails. Only a
// live session matching the recipient gets the best-effort consumption step.
func (s *Service) Pull(ctx context.Context, to messaging.Address, filter messaging.Filter, sessionID string) (Result, error) {
	envs, err := s.store.Inbox(ctx, to, filter)
	if err != nil {
		return Result{}, err
	}
	if s.sessionIsRecipient(sessionID, to) {
		for _, env := range envs {
			if err := s.store.Consume(ctx, env.ID, to); err != nil {
				log.Printf("api: inbox: consume message %s pulled by its recipient failed: %v", env.ID, err)
			}
		}
	}
	return Result{Messages: envs}, nil
}

func (s *Service) sessionIsRecipient(sessionID string, to messaging.Address) bool {
	if sessionID == "" || s.sessions == nil || !s.sessions.Live(sessionID) {
		return false
	}
	switch to.Kind {
	case messaging.KindSession:
		return to.ID == sessionID
	case messaging.KindAgent:
		agentID, err := s.sessions.LogicalAgentID(sessionID)
		return err == nil && agentID != "" && agentID == to.ID
	default:
		return false
	}
}
