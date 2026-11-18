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
