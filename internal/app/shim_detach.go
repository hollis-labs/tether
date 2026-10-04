//go:build !windows

package app

import (
	"context"
	"github.com/hollis-labs/agentkit/agentsessions"
	"github.com/hollis-labs/tether/internal/session"
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
		_ = s.MarkSessionOrphaned(id, "shim_gone")
		return
	}
	if result.Running {
		return
	}
	if err = host.stopProvider(ctx, receipt); err != nil {
		s.retainShim(row, &receipt, shimFailureCode(err))
		return
	}
	exit := result.Exit.Status
	state := session.StateCompleted
	if exit != 0 || result.Exit.Signal != 0 {
		state = session.StateFailed
	}
	_ = s.Store.UpdateSessionState(id, string(state), 0, &exit)
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
	s.shimDraining.Store(id, true)
	s.retainShim(shimRow, nil, ShutdownStopReason)
	// Agentkit closes only bridge stdin; bridge EOF disconnects without
	// closing the hosted provider's stdin or retiring its placement.
	_ = s.Manager.Stop(ctx, id)
	return true
}
