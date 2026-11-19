package app

import (
	"encoding/json"
	"log"
)

// KindSessionKillOrdered is published when a worker's reconnect handshake
// orders a session it holds killed, with why (CW-20261114-0168): before it, a
// welcome's kills left no trace and 26 live sessions died as plain exit 143s.
const KindSessionKillOrdered = "session.kill_ordered"

// End reasons recorded on a terminal session.state_changed (CW-20261114-0168).
// Every session end says who or what ended it.
const (
	// EndReasonWorkerExit: the agent on a worker exited without a stop or kill
	// from the hub.
	EndReasonWorkerExit = "worker_exit"
	// EndReasonStop: a stop was requested and gave no reason of its own.
	EndReasonStop = "stop"
	// EndReasonHubKill prefixes the reason a welcome ordered a kill
	// ("hub_kill:hub_state_killed", "hub_kill:unknown_to_hub").
	EndReasonHubKill = "hub_kill:"
	// EndReasonRuntimeExit: a local runtime ended and its library gave no reason.
	EndReasonRuntimeExit = "runtime_exit"
	// EndReasonNeverStarted: a session closed while still created.
	EndReasonNeverStarted = "never_started"
	// EndReasonTeamStop: a team stopped its member session.
	EndReasonTeamStop = "team_stop"
	// EndReasonShimRecovery: shim recovery found the session gone.
	EndReasonShimRecovery = "shim_recovery"
)

// recordWorkerKillOrder is the hub's KillOrdered hook: it remembers why the
// session's coming exit was ordered, for its terminal event, and publishes
// session.kill_ordered.
func (s *Service) recordWorkerKillOrder(workerID, sessionID, reason string) {
	s.killOrders.Store(sessionID, reason)
	raw, _ := json.Marshal(map[string]any{"worker_id": workerID, "reason": reason})
	if err := s.publishWorkerEvent(sessionID, KindSessionKillOrdered, raw); err != nil {
		log.Printf("app: publish %s for session %s: %v", KindSessionKillOrdered, sessionID, err)
	}
}

// workerEndReason says why a worker session ended: a requested stop (with its
// reason), a kill a welcome ordered, or the agent exiting on its own.
func (s *Service) workerEndReason(sessionID string, stopRequested bool, stopReason string) string {
	ordered, wasOrdered := s.killOrders.LoadAndDelete(sessionID)
	switch {
	case stopRequested && stopReason != "":
		return stopReason
	case stopRequested:
		return EndReasonStop
	case wasOrdered:
		return EndReasonHubKill + ordered.(string)
	}
	return EndReasonWorkerExit
}

// publishSessionEnded publishes the terminal session.state_changed of an end
// that has no other event.
func (s *Service) publishSessionEnded(id, logicalAgentID, from, to string, exit *int, reason string) {
	publishSessionEvent(s.Bus, id, logicalAgentID, "session.state_changed", sessionStateChangedPayload{From: from, To: to, ExitCode: exit, Reason: reason, Session: sessionFieldsFor(s.Store, id)})
}
