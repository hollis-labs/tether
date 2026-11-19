// Package conversation resolves a chat conversation to the Tether session
// that currently runs it (CW-20261118-0008, docs/conversations.md).
//
// The resolver reads only the Tether store. It must never import or call the
// chat host (libs/agent/sdk/chathost): terminal attachments invoke a target
// resolver while the chat host's lease lock is held. resolver_deps_test.go
// enforces the import rule.
package conversation

import (
	"context"
	"errors"

	"github.com/hollis-labs/tether/internal/store"
)

// ErrNotFound is returned when no conversation matches the target.
var ErrNotFound = errors.New("conversation not found")

// ErrStale is returned by Resolve for a session target that no longer runs its
// conversation (it was relaunched, resumed or replaced).
var ErrStale = errors.New("session no longer runs its conversation")

// Target is where a conversation runs now.
type Target struct {
	ConversationID string `json:"conversation_id"`
	Owner          string `json:"owner"`
	Agent          string `json:"agent"`
	Seat           string `json:"seat"`
	Kind           string `json:"kind"`
	Generation     uint64 `json:"generation"`
	SessionID      string `json:"session_id"`
	AttemptID      string `json:"attempt_id"`
	// WorkerID is the worker the session runs on; empty means this daemon.
	WorkerID string `json:"worker_id,omitempty"`
}

// Store is the read side of the Tether store the resolver needs.
type Store interface {
	Conversation(ctx context.Context, id string) (store.Conversation, error)
	ConversationForSession(ctx context.Context, sessionID string) (store.Conversation, int64, error)
	ConversationForSeat(ctx context.Context, seat string) (store.Conversation, error)
}

// Resolver maps conversation and session IDs to the current Target.
type Resolver struct {
	Store Store
}

// ByConversation returns the current target of conversation id.
func (r Resolver) ByConversation(ctx context.Context, id string) (Target, error) {
	if r.Store == nil || id == "" {
		return Target{}, ErrNotFound
	}
	c, err := r.Store.Conversation(ctx, id)
	return target(c, err)
}

// BySeat returns the current target of a seat's stable conversation. The chat
// wire (internal/chatwire) names an agent by its seat.
func (r Resolver) BySeat(ctx context.Context, seat string) (Target, error) {
	if r.Store == nil || seat == "" {
		return Target{}, ErrNotFound
	}
	c, err := r.Store.ConversationForSeat(ctx, seat)
	return target(c, err)
}

// BySession returns the current target of the conversation sessionID belongs
// to, and whether sessionID is that target. A session that has been relaunched
// or replaced still resolves, with current=false and the newer session in the
// target; a caller holding an attachment to it holds a stale generation.
func (r Resolver) BySession(ctx context.Context, sessionID string) (t Target, current bool, err error) {
	if r.Store == nil || sessionID == "" {
		return Target{}, false, ErrNotFound
	}
	c, _, err := r.Store.ConversationForSession(ctx, sessionID)
	t, err = target(c, err)
	return t, err == nil && t.SessionID == sessionID, err
}

// Resolve maps a UI target, a conversation ID or a session ID, to the
// conversation's current target. A session that has been superseded is
// ErrStale (with the current target), so a client holding it learns that its
// generation moved instead of being silently moved to the new session.
// Terminal attachments (internal/chatwire, CW-20261118-0012) call it while the
// chat host's lease lock is held; it reads only the Tether store.
func (r Resolver) Resolve(ctx context.Context, target string) (Target, error) {
	t, err := r.ByConversation(ctx, target)
	if !errors.Is(err, ErrNotFound) {
		return t, err
	}
	t, current, err := r.BySession(ctx, target)
	if err != nil {
		return Target{}, err
	}
	if !current {
		return t, ErrStale
	}
	return t, nil
}

func target(c store.Conversation, err error) (Target, error) {
	if errors.Is(err, store.ErrConversationNotFound) {
		return Target{}, ErrNotFound
	}
	if err != nil {
		return Target{}, err
	}
	return Target{
		ConversationID: c.ID,
		Owner:          c.Owner,
		Agent:          c.Agent,
		Seat:           c.Seat,
		Kind:           c.Kind,
		Generation:     uint64(c.Generation), //nolint:gosec // G115: CHECK (generation >= 1)
		SessionID:      c.SessionID,
		AttemptID:      c.AttemptID,
		WorkerID:       c.WorkerID,
	}, nil
}
