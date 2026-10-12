package app

import (
	"github.com/hollis-labs/substrate/harness/adapters/agentsessions"
	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/session"
	"github.com/hollis-labs/tether/internal/store"
)

func (s *Service) workerSession(id string) (*store.SessionRow, bool) {
	if s.Store == nil {
		return nil, false
	}
	row, err := s.Store.GetSession(id)
	return row, err == nil && row.WorkerID != ""
}

// workerSessionUnreachable reports a live-looking worker session whose worker
// is disconnected. The hub cannot tell "ended" from "out of reach" then, and a
// session only ends when the worker says so or an operator stops it.
func (s *Service) workerSessionUnreachable(id string) bool {
	row, ok := s.workerSession(id)
	if !ok || s.Workers == nil || session.State(row.State).Terminal() {
		return false
	}
	return !s.Workers.Connected(row.WorkerID)
}

func (s *Service) workerRuntimeHealth(row *store.SessionRow) (api.RuntimeHealthResult, bool) {
	if s.Workers == nil {
		return api.RuntimeHealthResult{}, false
	}
	state, alive := s.Workers.SessionState(row.WorkerID, row.ID)
	if !alive {
		return api.RuntimeHealthResult{}, false
	}
	live := agentsessions.LiveStateIdle
	if state == "running" {
		live = agentsessions.LiveStateProcessing
	}
	return api.RuntimeHealthResult{SessionID: row.ID, ProviderID: row.ProviderID, ProviderKind: row.ProviderKind, Caps: agentsessions.Capabilities{StreamingStdio: true}, Health: agentsessions.HealthStatus{Alive: true, PID: int(row.PID.Int64), State: live}}, true
}
