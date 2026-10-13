package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/session"
)

const sessionForceClosedEvent = "session.force_closed"

// ForceStopSession closes a session row that has no live worker or agent, so
// an orphan (for example a launch the hub gave up on while the worker started
// it later) cannot stay "running" forever. The row becomes killed with
// exit_code -1 (not an observed exit) and the reason is recorded on a
// session.force_closed event. A session a live worker or local runtime still
// holds is refused with api.ErrSessionLive unless opts.AllowLive is set.
func (s *Service) ForceStopSession(ctx context.Context, id string, opts api.ForceStopOptions) (api.ForceStopResult, error) {
	res := api.ForceStopResult{SessionID: id, To: string(session.StateKilled)}
	row, err := s.Store.GetSession(id)
	if err != nil {
		return res, err
	}
	res.From = row.State
	if session.State(row.State).Terminal() {
		return res, fmt.Errorf("%w: %s", api.ErrSessionAlreadyTerminal, row.State)
	}
	res.WasLive = s.sessionHeldLive(row.ID, row.WorkerID)
	if res.WasLive && !opts.AllowLive {
		return res, api.ErrSessionLive
	}
	reason := opts.Reason
	if reason == "" {
		reason = "operator force close"
	}
	res.Reason = reason
	exit := -1
	if err = s.Store.UpdateSessionState(id, string(session.StateKilled), 0, &exit); err != nil {
		return res, err
	}
	if strings.TrimSpace(row.LogicalAgentID) != "" {
		s.revokeActorBindingIfCurrent(id, row.LogicalAgentID)
	}
	s.releaseWorkerIdentity(id)
	if s.Registry != nil {
		_, _ = s.Registry.RevokeSessionBindings(ctx, id)
	}
	payload, _ := json.Marshal(map[string]any{"from": row.State, "to": res.To, "reason": reason, "was_live": res.WasLive, "worker_id": row.WorkerID})
	if s.Bus == nil {
		_, _, err = s.Store.InsertEventContext(ctx, events.ScopeSession, id, sessionForceClosedEvent, string(payload))
		return res, err
	}
	return res, s.Bus.Publish(ctx, events.Event{Scope: events.ScopeSession, SessionID: id, LogicalAgentID: row.LogicalAgentID, Kind: sessionForceClosedEvent, PayloadJSON: string(payload)})
}

// sessionHeldLive reports whether a connected worker reports the session, or
// (for a hub-local session) the Manager still has a runtime for it.
func (s *Service) sessionHeldLive(id, workerID string) bool {
	if workerID != "" {
		if s.Workers == nil || !s.Workers.Connected(workerID) {
			return false
		}
		_, alive := s.Workers.SessionState(workerID, id)
		return alive
	}
	if s.Manager == nil {
		return false
	}
	_, ok := s.Manager.Get(id)
	return ok
}
