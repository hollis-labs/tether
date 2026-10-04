//go:build !windows

package app

import (
	"context"
	"github.com/hollis-labs/agentkit/agentsessions"
	"github.com/hollis-labs/substrate/harness/shim"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/session"
	"github.com/hollis-labs/tether/internal/shimhost"
	"github.com/hollis-labs/tether/internal/store"
	"time"
)

func (s *Service) settleShimBridgeExit(id string) {
	if _, draining := s.shimDraining.Load(id); draining {
		return
	}
	row, err := s.Store.SessionShim(context.Background(), id)
	if err != nil || s.stops.requested(id) {
		return
	}
	receipt, err := loadShimReceipt(row)
	if err != nil {
		s.retainShim(row, nil, shimFailureCode(err))
		return
	}
	host, err := s.shimHost()
	if err != nil {
		s.retainShim(row, &receipt, shimFailureCode(err))
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := host.inspect(ctx, receipt)
	if err != nil {
		s.retainShim(row, &receipt, shimFailureCode(err))
		return
	}
	if result.Gone {
		if err := host.stopProvider(ctx, receipt); err != nil {
			s.retainShim(row, &receipt, shimFailureCode(err))
			return
		}
		_ = s.MarkSessionOrphaned(id, "shim_gone")
		return
	}
	if result.Running {
		s.retainShim(row, &receipt, "bridge_disconnected")
		return
	}
	if err = host.stopProvider(ctx, receipt); err != nil {
		s.retainShim(row, &receipt, shimFailureCode(err))
		return
	}
	s.completeShimExit(row, receipt, result.Exit)
}

func shimBridgeTerminal(db *store.Store, id string, state agentsessions.State) bool {
	if db == nil || state != agentsessions.StateDone && state != agentsessions.StateFailed {
		return false
	}
	_, err := db.SessionShim(context.Background(), id)
	return err == nil
}

func (s *Service) shimSessionRetained(id string) bool {
	if s.Store == nil {
		return false
	}
	if _, err := s.Store.SessionShim(context.Background(), id); err != nil {
		return false
	}
	row, err := s.Store.GetSession(id)
	return err != nil || !session.State(row.State).Terminal()
}

func (s *Service) detachShimSession(ctx context.Context, id string) bool {
	shimRow, err := s.Store.SessionShim(ctx, id)
	if err != nil {
		return false
	}
	draining := any(true)
	if value, ok := s.shimBindingWait.Load(id); ok {
		draining = value
	}
	s.shimDraining.Store(id, draining)
	s.retainShim(shimRow, nil, ShutdownStopReason)
	// Agentkit closes only bridge stdin; bridge EOF disconnects without
	// closing the hosted provider's stdin or retiring its placement.
	_ = s.Manager.Stop(ctx, id)
	return true
}

func (s *Service) completeShimExit(row store.SessionShimRow, receipt shimhost.Receipt, result shim.Exit) {
	exit := result.Status
	state := session.StateCompleted
	if exit != 0 || result.Signal != 0 {
		state = session.StateFailed
	}
	change, changed, err := s.Store.CompleteSessionShim(context.Background(), row.SessionID, string(state), exit)
	if err != nil {
		s.retainShim(row, &receipt, "outcome_unknown")
		return
	}
	if !changed {
		return
	}
	publishSessionEvent(s.Bus, row.SessionID, change.LogicalAgentID, events.KindSessionStateChanged, sessionStateChangedPayload{From: change.From, To: string(state), ExitCode: &exit, Reason: "provider_exit"})
	if host, err := s.shimHost(); err == nil {
		if cleanup, ok := host.cleanup.LoadAndDelete(row.SessionID); ok {
			cleanup.(func())()
		}
	}
	s.shimDiagnostic(row.SessionID, &receipt, string(state), "provider_exit")
}
