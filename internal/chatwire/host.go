package chatwire

import (
	"context"
	"errors"
	"fmt"

	"github.com/hollis-labs/libs/agent/sdk/chathost"
	"github.com/hollis-labs/libs/agent/sdk/session"
	"github.com/hollis-labs/libs/mesh/core/runtimecontract"
)

// syncHost moves the chat host conversation of want's Tether conversation to want: it registers the agent and creates
// the conversation the first time, activates a fenced record, and steps the generation one at a time (chathost's CAS
// only advances by one, naming the previous binding as parent). The Tether conversations row is the authority, so
// chathost only ever follows it; a chat host binding ahead of it, or a different attempt at the same generation, is
// ErrHostDrift.
//
// Generations chathost skipped (a conversation first seen at generation 5, or a daemon that was down across two
// relaunches) are stepped through with placeholder attempt IDs: chathost requires an attempt per generation and never
// reuses one, and only the current binding is ever compared.
func syncHost(ctx context.Context, h *chathost.Host, want runtimecontract.Binding) error {
	sc := session.Scope{OwnerID: want.OwnerID, OperativeID: want.OperativeID, ConversationID: want.ConversationID}
	rec, err := h.Load(ctx, sc)
	if errors.Is(err, session.ErrNotFound) {
		agent := chathost.Agent{ID: want.OperativeID, Owner: want.OwnerID, Seat: want.SeatID, Kind: chathost.Stable, Supervisor: chathost.Tether}
		if err := h.RegisterAgent(ctx, agent); err != nil && !errors.Is(err, session.ErrConflict) {
			return fmt.Errorf("chat host register agent: %w", err)
		}
		first := want
		if want.Generation > 1 {
			first.Generation, first.AttemptID = 1, catchUpAttempt(want, 1)
		}
		rec, err = h.Create(ctx, session.Record{Binding: first, Phase: session.Fenced})
	}
	if err != nil {
		return fmt.Errorf("chat host load: %w", err)
	}
	for {
		b := rec.Binding
		switch {
		case rec.Phase == session.Fenced:
			rec, err = h.CompareAndSwap(ctx, sc, rec.Version, session.Record{Binding: b, Parent: rec.Parent, Phase: session.Active, Reason: rec.Reason})
		case b == want:
			return nil
		case b.Generation >= want.Generation:
			return fmt.Errorf("%w: chat host at generation %d attempt %s, Tether at generation %d attempt %s",
				ErrHostDrift, b.Generation, b.AttemptID, want.Generation, want.AttemptID)
		default:
			next := want
			if b.Generation+1 < want.Generation {
				next.Generation = b.Generation + 1
				next.AttemptID = catchUpAttempt(want, next.Generation)
			}
			parent := b
			rec, err = h.CompareAndSwap(ctx, sc, rec.Version, session.Record{Binding: next, Parent: &parent, Phase: session.Fenced, Reason: session.Recovery})
		}
		if err != nil {
			return fmt.Errorf("chat host generation step: %w", err)
		}
	}
}

// catchUpAttempt is the placeholder attempt for a generation chathost never saw run.
func catchUpAttempt(want runtimecontract.Binding, g runtimecontract.Generation) runtimecontract.AttemptID {
	return runtimecontract.AttemptID(fmt.Sprintf("%s.catchup.%d", want.ConversationID, g))
}
