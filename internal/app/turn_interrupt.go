package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hollis-labs/agentkit/agentsessions"
	"github.com/hollis-labs/tether/internal/events"
)

// RoutingInterruptWired reports host wiring for a catalog provider. Capability
// queries must also require the runtime descriptor's cancel_turn advertisement.
func (s *Service) RoutingInterruptWired(providerID string) bool {
	return s.Manager != nil && s.Store != nil && s.factories[providerID] != nil
}

// CancelTurnAndWait interrupts the submission open at entry, then waits for its
// bound reducer Output (terminal or failure). It never submits a reply itself.
// actor is the verified caller identity supplied by the reply surface.
//
// A provisional submission is refused immediately. A turn that ends or changes
// before the gated runtime call is refused without canceling a successor.
// Runtime acknowledgement alone is insufficient: completion must belong to
// the captured marker. Call from a caller goroutine, never a runtime callback.
func (s *Service) CancelTurnAndWait(ctx context.Context, sessionID, actor string) (result TurnInterruptResult, err error) {
	if strings.TrimSpace(actor) == "" {
		return result, errors.New("turn interruption requires a caller identity")
	}
	if s.Manager == nil {
		return result, agentsessions.ErrSessionNotRunning
	}
	if _, ok := s.Manager.Get(sessionID); !ok {
		return result, agentsessions.ErrSessionNotRunning
	}
	state, ok := s.SessionTurnOutputState(sessionID)
	if ok {
		result.TurnID, _ = state.CurrentTurn()
	}
	defer func() {
		outcome := "completed"
		if err != nil {
			outcome = "error"
			var refusal *TurnInterruptRefusal
			if errors.As(err, &refusal) {
				outcome = string(refusal.Reason)
			}
		}
		payload := events.TurnInterruptEvent{Actor: actor, SessionID: sessionID, TurnID: result.TurnID,
			OutputTurnID: result.OutputTurnID, Result: outcome}
		if err != nil {
			payload.Error = err.Error()
		}
		if auditErr := s.auditTurnInterrupt(events.KindSessionTurnInterruptCompleted, payload); auditErr != nil {
			err = errors.Join(err, fmt.Errorf("audit turn interruption result: %w", auditErr))
		}
	}()
	if !ok || result.TurnID == "" {
		return result, &TurnInterruptRefusal{Reason: TurnInterruptNoTurn, SessionID: sessionID}
	}
	return s.cancelTurnAndWait(ctx, sessionID, actor, state, result.TurnID)
}

// The intended ID is captured before acquiring the gate. Keeping this separate
// makes the snapshot-to-gate interleaving explicit and testable.
func (s *Service) cancelTurnAndWait(ctx context.Context, sessionID, actor string, state TurnOutputState, intended string) (TurnInterruptResult, error) {
	return s.cancelTurnAndWaitWithClock(ctx, sessionID, actor, state, intended, realInterruptClock{})
}

func (s *Service) cancelTurnAndWaitWithClock(ctx context.Context, sessionID, actor string, state TurnOutputState, intended string, clock interruptClock) (TurnInterruptResult, error) {
	result := TurnInterruptResult{TurnID: intended}
	var done <-chan struct{}
	err := func() error {
		unlock := state.LockSubmission()
		defer unlock()
		if err := ctx.Err(); err != nil {
			return err
		}
		current, ch := state.CurrentTurn()
		if current == "" {
			return &TurnInterruptRefusal{Reason: TurnInterruptNoTurn, SessionID: sessionID, TurnID: intended}
		}
		if current != intended {
			return &TurnInterruptRefusal{Reason: TurnInterruptSuperseded, SessionID: sessionID, TurnID: intended}
		}
		if !state.TurnAccepted(intended) {
			// The reader can complete the turn between the snapshot and the
			// acceptance check; completion does not take the submission gate.
			current, _ = state.CurrentTurn()
			if current == "" {
				return &TurnInterruptRefusal{Reason: TurnInterruptNoTurn, SessionID: sessionID, TurnID: intended}
			}
			if current != intended {
				return &TurnInterruptRefusal{Reason: TurnInterruptSuperseded, SessionID: sessionID, TurnID: intended}
			}
			return &TurnInterruptRefusal{Reason: TurnInterruptNotStarted, SessionID: sessionID, TurnID: intended}
		}
		done = ch
		if err := s.auditTurnInterrupt(events.KindSessionTurnInterruptRequested, events.TurnInterruptEvent{
			Actor: actor, SessionID: sessionID, TurnID: intended, Result: "requested",
		}); err != nil {
			return fmt.Errorf("audit turn interruption request: %w", err)
		}
		deadline := clock.Now().Add(2 * time.Second)
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			current, _ := state.CurrentTurn()
			if current == "" {
				return &TurnInterruptRefusal{Reason: TurnInterruptNoTurn, SessionID: sessionID, TurnID: intended}
			}
			if current != intended {
				return &TurnInterruptRefusal{Reason: TurnInterruptSuperseded, SessionID: sessionID, TurnID: intended}
			}
			err := s.Manager.InterruptTurn(ctx, sessionID)
			if errors.Is(err, agentsessions.ErrInterruptUnsupported) {
				return &TurnInterruptRefusal{Reason: TurnInterruptUnsupported, SessionID: sessionID, TurnID: intended}
			}
			if !errors.Is(err, agentsessions.ErrTurnNotStarted) {
				return err
			}
			remaining := deadline.Sub(clock.Now())
			if remaining <= 0 {
				return &TurnInterruptRefusal{Reason: TurnInterruptNotStarted, SessionID: sessionID, TurnID: intended}
			}
			if err := clock.Wait(ctx, min(10*time.Millisecond, remaining)); err != nil {
				return err
			}
		}
	}()
	if err != nil {
		return result, err
	}
	select {
	case <-done:
		id, matched := state.CompletedTurn(intended)
		if !matched {
			return result, &TurnInterruptRefusal{Reason: TurnInterruptSuperseded, SessionID: sessionID, TurnID: intended}
		}
		result.OutputTurnID = id
		return result, nil
	case <-ctx.Done():
		return result, ctx.Err()
	}
}

func (s *Service) auditTurnInterrupt(kind string, payload events.TurnInterruptEvent) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if s.Bus != nil {
		return s.Bus.Publish(context.Background(), events.Event{Scope: events.ScopeSession,
			SessionID: payload.SessionID, Kind: kind, PayloadJSON: string(data)})
	}
	if s.Store != nil {
		_, _, err := s.Store.InsertEvent(events.ScopeSession, payload.SessionID, kind, string(data))
		return err
	}
	return errors.New("turn interruption audit storage is unavailable")
}
