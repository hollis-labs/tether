package app

import (
	"context"
	"errors"

	"github.com/hollis-labs/tether/internal/session"
	"github.com/hollis-labs/tether/internal/store"
)

// reconcileRecordedExit settles an observed exit, never inferring remote
// liveness from the hub's Manager. Serialize with launch admission and recheck
// the durable snapshot at commit; a runtime or pending launch always wins.
func (s *Service) reconcileRecordedExit(ctx context.Context, id string) (bool, error) {
	s.launchMu.Lock()
	defer s.launchMu.Unlock()
	if gate := s.launches[id]; gate != nil && gate.refs > 0 {
		return false, nil
	}
	row, err := s.Store.GetSession(id)
	if err != nil {
		return false, err
	}
	terminal := session.State(row.State).Terminal()
	if !terminal && (row.State != "running" || row.PID.Int64 != 0 || !row.EndedAt.Valid || row.EndedAt.String == "" || !row.ExitCode.Valid) {
		return false, nil
	}
	// A retained shim has its own identity-checked recovery and stop
	// authority, including its existing terminal-stop return contract.
	if _, err := s.Store.SessionShim(ctx, id); err == nil {
		return false, nil
	} else if !errors.Is(err, store.ErrSessionShimNotFound) {
		return false, err
	}
	if terminal {
		// A failed launch acknowledgement can coexist with a live worker.
		// Preserve explicit stop routing when that worker still holds it.
		return row.WorkerID == "" || !s.sessionHeldLive(id, row.WorkerID), nil
	}
	if s.sessionHeldLive(id, row.WorkerID) {
		return false, nil
	}
	changed, err := s.Store.ReconcileRecordedExit(ctx, *row)
	if err != nil || !changed {
		return false, err
	}
	to := string(session.StateFailed)
	if row.ExitCode.Int64 == 0 {
		to = string(session.StateCompleted)
	}
	s.publishSessionStateChange(store.SessionStateChange{SessionID: id, LogicalAgentID: row.LogicalAgentID, From: row.State, To: to, Reason: "recorded_exit_reconciled"})
	s.releaseWorkerIdentity(id)
	if s.Registry != nil {
		_, err = s.Registry.RevokeSessionBindings(ctx, id)
	}
	return true, err
}

func (s *Service) reconcileRecordedExits(ctx context.Context) error {
	ids, err := s.Store.RecordedExitCandidates(ctx)
	if err != nil {
		return err
	}
	var failures []error
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		if _, err := s.reconcileRecordedExit(ctx, id); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (s *Service) sessionLaunchPending(id string) bool {
	s.launchMu.Lock()
	defer s.launchMu.Unlock()
	return s.launches[id] != nil && s.launches[id].refs > 0
}
