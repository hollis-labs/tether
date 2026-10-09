//go:build !windows

package shimcodex

import (
	"context"
	"encoding/json"
	"math"
	"strconv"
	"strings"

	"github.com/hollis-labs/substrate/harness/shim"
)

type DeliveryHandler func(context.Context, State) (Projection, error)

// DeliveryStore commits projection, inbox removal and checked durable output
// references together. Ordinary protocol Commit cannot issue this capability.
type DeliveryStore interface {
	CommitDelivery(context.Context, State, State) error
}

// BuildDeliveryProjection interprets only the immutable durable reader input.
// Unrecognized output obligations and unanswered grant callbacks stay pending.
func BuildDeliveryProjection(state State) (Projection, error) {
	p := Projection{Version: ProjectionVersion, Binding: state.Binding, JournalIdentity: state.Binding.Journal, ProtocolRevision: state.Revision}
	if state.Delivery != nil {
		raw, err := json.Marshal(state.Delivery)
		if err != nil {
			return p, err
		}
		p, err = DecodeProjection(raw)
		if err != nil || p.Binding != state.Binding {
			return p, fail("projection_invalid")
		}
		// Completed delivery is already attested in the canonical CAS record.
		// Carry its digest/cursor rather than accumulating every historical turn
		// and delta forever. Open turns retain all source references they need.
		settled := p.ActiveTurnID == "" && p.Terminal == nil && p.DeliveredHighWater == p.AcceptedSourceCursor && len(p.PartialBytes) == 0
		for _, turn := range p.Turns {
			if turn.Phase == "open" || turn.OutputAcceptanceID == "" {
				settled = false
			}
		}
		if settled && p.DeliveredHighWater != "" && len(state.Inbox) != 0 {
			p.BaseSourceCursor = p.DeliveredHighWater
			p.BaseReceiptSHA256 = digest(raw)
			p.Sources = nil
			p.Turns = nil
		}
		// Complete message identities begin at the durable carry's start,
		// rather than at the end of the previously accepted physical frame.
		p.StdoutOffset = p.PartialStart
		p.PartialBytes = nil
	}
	var err error
	for _, event := range state.Inbox {
		p, err = ProjectFrozenSource(p, event)
		if err != nil {
			return p, err
		}
		source := &p.Sources[len(p.Sources)-1]
		if source.Kind == "server_request" {
			for _, request := range state.ServerRequests {
				if request.Source == event.Identity && request.Written {
					source.Disposition = "protocol_only"
				}
			}
		}
		if source.Kind == "provider_exit" && state.Exit != nil && event.Identity == state.Binding.Journal+":exit:"+state.ExitCursor {
			var exit shim.Exit
			if json.Unmarshal(event.Raw, &exit) != nil || exit.Status != state.Exit.Status || exit.Signal != state.Exit.Signal || exit.Cause != state.Exit.Cause {
				return p, fail("source_conflict")
			}
			source.Disposition = "projected"
			p.Terminal = &ProjectedTerminal{SourceEventID: event.Identity, Cursor: event.Cursor, PayloadSHA256: source.PayloadSHA256, Status: exit.Status, Signal: strconv.Itoa(exit.Signal), Cause: exit.Cause, Disposition: "authenticated_provider_exit"}
		}
		if source.Disposition == "retained_unsupported" {
			return p, fail("output_unsupported")
		}
	}
	p.ProtocolRevision = state.Revision
	p.ReplayHighWater = state.ReplayHighWater
	p.StdoutOffset = state.StreamOffset
	p.PartialStart = state.PartialStart
	p.PartialBytes = append([]byte(nil), state.Partial...)
	return p, ValidateProjection(p)
}

func ProjectedTurnText(turn ProjectedTurn) string {
	final := false
	for _, item := range turn.Items {
		if item.Phase == "final_answer" {
			final = true
		}
	}
	var parts []string
	for _, item := range turn.Items {
		if !final || item.Phase == "final_answer" {
			parts = append(parts, item.TextBytes)
		}
	}
	return strings.Join(parts, "\n")
}

// DeliverInbox never holds the reader lock during host output publication.
// A concurrent protocol change leaves the inbox intact and replays identical
// output identities. An ambiguous atomic commit poisons input as usual.
func (e *Engine) DeliverInbox(ctx context.Context, deliver DeliveryHandler) error {
	e.mu.Lock()
	if e.poisoned {
		e.mu.Unlock()
		return fail("checkpoint_unknown")
	}
	store, ok := e.store.(DeliveryStore)
	if !ok {
		e.mu.Unlock()
		return fail("output_unsupported")
	}
	observed := clone(e.state)
	e.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(observed.Inbox) == 0 || deliver == nil {
		return nil
	}
	p, err := deliver(ctx, observed)
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.poisoned {
		return fail("checkpoint_unknown")
	}
	if e.state.Revision != observed.Revision {
		return fail("delivery_changed")
	}
	if e.state.Revision == math.MaxUint64 {
		return fail("counter_exhausted")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	next := clone(observed)
	next.Revision++
	p.ProtocolRevision = next.Revision
	p.DeliveredHighWater = observed.Cursor
	next.Delivery = &p
	next.Inbox = nil
	if err := validateState(next, e.limits); err != nil {
		return err
	}
	if err := store.CommitDelivery(ctx, observed, next); err != nil {
		e.poisoned = true
		return fail("checkpoint_unknown")
	}
	e.state = next
	return nil
}
