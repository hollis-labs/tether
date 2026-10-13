package app

import (
	"context"
	"time"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/events"
)

const (
	// stallDenialThreshold is how many refused tool actions since the last
	// turn output mark a session stalled.
	stallDenialThreshold = 3
	// stallIdleAfter is how long a session with at least one refusal and no
	// events at all is quiet before it counts as stalled.
	stallIdleAfter = 10 * time.Minute

	stallReasonDenials       = "repeated_tool_denials"
	stallReasonDeniedAndIdle = "tool_denied_and_idle"
)

// classifyStall turns activity counters into the stall signal.
func classifyStall(denied int, idle time.Duration) api.StallStatus {
	st := api.StallStatus{DeniedSinceOutput: denied, IdleSeconds: int64(idle / time.Second)}
	switch {
	case denied >= stallDenialThreshold:
		st.Stalled, st.Reason = true, stallReasonDenials
	case denied > 0 && idle >= stallIdleAfter:
		st.Stalled, st.Reason = true, stallReasonDeniedAndIdle
	}
	return st
}

// SessionStall reports the stalled-session signal for id, derived from the
// durable event log. ok is false when the activity cannot be read.
func (s *Service) SessionStall(id string) (api.StallStatus, bool) {
	if s.Store == nil {
		return api.StallStatus{}, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	act, err := s.Store.SessionActivity(ctx, id)
	if err != nil {
		return api.StallStatus{}, false
	}
	var idle time.Duration
	if !act.LastEventAt.IsZero() {
		idle = time.Since(act.LastEventAt)
	}
	return classifyStall(act.DeniedSinceOutput, idle), true
}

// notifyStallOnDenial publishes session.stalled exactly when the denial
// count reaches the threshold, so operators are told once per stall.
func (s *Service) notifyStallOnDenial(sessionID, logicalAgentID string) {
	if s.Store == nil || s.Bus == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	act, err := s.Store.SessionActivity(ctx, sessionID)
	if err != nil || act.DeniedSinceOutput != stallDenialThreshold {
		return
	}
	publishSessionEventContext(ctx, s.Bus, sessionID, logicalAgentID, events.KindSessionStalled, struct {
		Reason string `json:"reason"`
		Denied int    `json:"denied"`
	}{stallReasonDenials, act.DeniedSinceOutput})
}
