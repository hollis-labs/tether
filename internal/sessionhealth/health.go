// Package sessionhealth emits session.health_changed (schema
// session_health_changed@1, architect ruling session_usage_and_health_kinds_2026_10_16).
//
// Tether has no health state of its own, so this tracker derives one only from
// signals Tether already publishes, never from a clock or a guess, and emits a
// transition only when the state changes. It is an events.Observer: it reads
// the events other code publishes, so no producer changed.
//
// Transition table (signal event -> change; every other pairing is no change):
//
//	session.state_changed to=running          unknown            -> healthy    [session_running]
//	session.stalled                           unknown|healthy    -> degraded   [repeated_tool_denials]
//	provider.session_lost                     unknown|healthy    -> degraded   [provider_session_lost]
//	session.turn_output kind=final            degraded           -> healthy    [turn_output_resumed]
//	session.state_changed to=failed           unknown|healthy|degraded -> unhealthy [runtime_failed]
//	session.state_changed to=orphaned         unknown|healthy|degraded -> unhealthy [runtime_orphaned]
//	session.shim_status detached/outcome_unknown  any but unhealthy -> unhealthy [shim_outcome_unknown]
//
// unhealthy is terminal: nothing leaves it. A completed or killed session (a
// requested, clean end) changes nothing. The tracker forgets a session when it
// ends, including a failed or orphaned end, so a late signal for an ended
// session (a stray turn output, a recovery event) starts again from unknown and
// carries tracker_restart. The tracker is in memory. After a
// restart the first transition it makes for a session starts from unknown, and
// when that first signal is not the session starting it carries the extra
// reason tracker_restart so a consumer does not read it as a fresh change.
// Reaper inactivity is deliberately not a signal here.
package sessionhealth

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/hollis-labs/tether/internal/events"
)

// States of session_health_changed@1.
const (
	Unknown   = "unknown"
	Healthy   = "healthy"
	Degraded  = "degraded"
	Unhealthy = "unhealthy"
)

// Closed reason codes (^[a-z][a-z0-9_]*$).
const (
	ReasonSessionRunning      = "session_running"
	ReasonRepeatedToolDenials = "repeated_tool_denials"
	ReasonProviderSessionLost = "provider_session_lost"
	ReasonTurnOutputResumed   = "turn_output_resumed"
	ReasonRuntimeFailed       = "runtime_failed"
	ReasonRuntimeOrphaned     = "runtime_orphaned"
	ReasonShimOutcomeUnknown  = "shim_outcome_unknown"
	ReasonTrackerRestart      = "tracker_restart"
	payloadSchema             = "session_health_changed@1"
	maxTracked                = 8192
	publishTimeout            = 5 * time.Second
	shimStatusDetached        = "detached"
	shimReasonOutcomeUnknown  = "outcome_unknown"
	stateRunning              = "running"
	turnOutputFinal           = "final"
)

type signal int

const (
	sigNone signal = iota
	sigRunning
	sigStalled
	sigSessionLost
	sigTurnOutput
	sigRuntimeFailed
	sigRuntimeOrphaned
	sigShimOutcomeUnknown
)

// Payload is the session_health_changed@1 payload.
type Payload struct {
	From    string    `json:"from"`
	To      string    `json:"to"`
	Reasons []string  `json:"reasons,omitempty"`
	Since   time.Time `json:"since"`
}

// Tracker is the observer. The zero value is not usable; use New.
type Tracker struct {
	// pubMu orders emissions: a transition is computed and published under it, so
	// two signals for one session reach the bus in the order they were applied
	// (their from/to chain is never reversed by sequence). Health events are
	// rare, so one lock for all sessions costs nothing.
	pubMu  sync.Mutex
	mu     sync.Mutex
	states map[string]string
	pub    events.Publisher
	now    func() time.Time
}

var _ events.Observer = (*Tracker)(nil)

// New returns a tracker. SetPublisher must be called before events arrive that
// should produce transitions.
func New() *Tracker {
	return &Tracker{states: map[string]string{}, now: time.Now}
}

// SetPublisher gives the tracker the bus it emits on (the bus is built after it).
func (t *Tracker) SetPublisher(p events.Publisher) {
	t.mu.Lock()
	t.pub = p
	t.mu.Unlock()
}

// Observe implements events.Observer.
func (t *Tracker) Observe(ctx context.Context, e events.Event) {
	if e.SessionID == "" {
		return
	}
	sig, terminal := classify(e)
	if sig == sigNone && !terminal {
		return
	}
	t.pubMu.Lock()
	defer t.pubMu.Unlock()
	var out *Payload
	t.mu.Lock()
	if sig != sigNone {
		out = t.transition(e.SessionID, sig)
	}
	if terminal {
		delete(t.states, e.SessionID)
	}
	pub := t.pub
	t.mu.Unlock()
	if out == nil || pub == nil {
		return
	}
	data, err := json.Marshal(out)
	if err != nil {
		return
	}
	ev := events.Event{Scope: events.ScopeSession, SessionID: e.SessionID, LogicalAgentID: e.LogicalAgentID, Kind: events.KindSessionHealthChanged, PayloadJSON: string(data)}
	ev.Envelope.Class = events.Class{Category: "state", PayloadSchema: payloadSchema}
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), publishTimeout)
	defer cancel()
	_ = pub.Publish(pctx, ev)
}

// transition applies sig to the session's tracked state and returns the payload
// to emit, or nil for no change. mu must be held.
func (t *Tracker) transition(id string, sig signal) *Payload {
	from, tracked := t.states[id]
	if !tracked {
		from = Unknown
		if len(t.states) >= maxTracked {
			clear(t.states)
		}
	}
	to, reason := next(from, sig)
	if to == "" || to == from {
		return nil
	}
	reasons := []string{reason}
	// First transition we make for a session whose start we did not see (the
	// tracker restarted under it): flag it.
	if !tracked && sig != sigRunning {
		reasons = append(reasons, ReasonTrackerRestart)
	}
	t.states[id] = to
	return &Payload{From: from, To: to, Reasons: reasons, Since: t.now().UTC()}
}

// next is the transition table. It returns "" for no change.
func next(from string, sig signal) (to, reason string) {
	if from == Unhealthy {
		return "", ""
	}
	switch sig {
	case sigRunning:
		if from == Unknown {
			return Healthy, ReasonSessionRunning
		}
	case sigStalled:
		if from == Unknown || from == Healthy {
			return Degraded, ReasonRepeatedToolDenials
		}
	case sigSessionLost:
		if from == Unknown || from == Healthy {
			return Degraded, ReasonProviderSessionLost
		}
	case sigTurnOutput:
		if from == Degraded {
			return Healthy, ReasonTurnOutputResumed
		}
	case sigRuntimeFailed:
		return Unhealthy, ReasonRuntimeFailed
	case sigRuntimeOrphaned:
		return Unhealthy, ReasonRuntimeOrphaned
	case sigShimOutcomeUnknown:
		return Unhealthy, ReasonShimOutcomeUnknown
	case sigNone:
	}
	return "", ""
}

// classify turns a published event into a signal, and reports whether it ends
// the session (so its tracked state can be dropped after the signal is applied).
func classify(e events.Event) (sig signal, terminal bool) {
	switch e.Kind {
	case events.KindSessionStateChanged:
		var p struct {
			To string `json:"to"`
		}
		if json.Unmarshal([]byte(e.PayloadJSON), &p) != nil {
			return sigNone, false
		}
		switch p.To {
		case stateRunning:
			return sigRunning, false
		case "failed":
			return sigRuntimeFailed, true
		case "orphaned":
			return sigRuntimeOrphaned, true
		case "completed", "killed":
			return sigNone, true
		}
	case events.KindSessionStalled:
		return sigStalled, false
	case events.KindProviderSessionLost:
		return sigSessionLost, false
	case events.KindSessionTurnOutput:
		var p struct {
			Kind string `json:"kind"`
		}
		if json.Unmarshal([]byte(e.PayloadJSON), &p) == nil && p.Kind == turnOutputFinal {
			return sigTurnOutput, false
		}
	case events.KindSessionShimStatus:
		var p struct {
			State  string `json:"state"`
			Reason string `json:"reason"`
		}
		if json.Unmarshal([]byte(e.PayloadJSON), &p) == nil && p.State == shimStatusDetached && p.Reason == shimReasonOutcomeUnknown {
			return sigShimOutcomeUnknown, false
		}
	}
	return sigNone, false
}
