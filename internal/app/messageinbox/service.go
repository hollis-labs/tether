// Package messageinbox owns an inbox pull and its optional recipient-session
// consumption. Transports supply decoded addresses and session observations.
package messageinbox

import (
	"context"
	"log"

	"github.com/hollis-labs/libs/mesh/core/messaging"

	"github.com/hollis-labs/tether/internal/store"
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
	// BoundToActor reports whether actorID's current registry binding resolves to exactly
	// this session: the rule the mailbox wake already uses to pick the session for an
	// actor whose mailbox URN is not its logical agent id.
	BoundToActor(sessionID, actorID string) bool
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
// live session matching the recipient gets the best-effort consumption step, and
// the same session gets unread messages only: what it already read is settled, not
// handed back as new and consumed again (CW-20261021-0029). Operator pulls are the
// plain pull.
func (s *Service) Pull(ctx context.Context, to messaging.Address, filter messaging.Filter, sessionID string) (Result, error) {
	recipient := s.sessionIsRecipient(sessionID, to)
	pullCtx := ctx
	if recipient {
		pullCtx = store.WithUnreadOnlyInbox(ctx)
	}
	envs, err := s.store.Inbox(pullCtx, to, filter)
	if err != nil {
		return Result{}, err
	}
	if recipient {
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
		if to.SubID != "" {
			// An agent instance mailbox (msg://agent/<authority>/<agent>/<session id>) belongs
			// to that one session. Another session of the same logical agent is not its
			// recipient, and the agent's binding is not the question (CW-20261023-0018).
			agentID, err := s.sessions.LogicalAgentID(sessionID)
			return err == nil && to.SubID == sessionID && agentID != "" && agentID == to.ID
		}
		agentID, err := s.sessions.LogicalAgentID(sessionID)
		if err == nil && agentID != "" && agentID == to.ID {
			return true
		}
		// An actor whose mailbox URN is not its logical agent id (an agt_ id) is still this
		// session's recipient when the actor's current binding resolves to this session.
		return s.sessions.BoundToActor(sessionID, to.ID)
	default:
		return false
	}
}
