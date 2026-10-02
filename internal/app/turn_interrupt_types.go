package app

import (
	"context"
	"fmt"
	"github.com/hollis-labs/go-agent-wrapper/turnoutput"

	"github.com/hollis-labs/agentkit/agentsessions"
)

// TurnInterruptResult identifies the submission that ended and its bound
// reducer output. ACP may use a runtime output ID distinct from the stable
// Tether submission ID.
type TurnInterruptResult struct {
	TurnID       string          `json:"turn_id"`
	OutputTurnID string          `json:"output_turn_id"`
	OutputKind   turnoutput.Kind `json:"output_kind"`
	StopReason   string          `json:"stop_reason"`
}

// TurnInterruptRefusalReason is a machine-readable refusal to interrupt.
type TurnInterruptRefusalReason string

const (
	TurnInterruptUnsupported  TurnInterruptRefusalReason = "unsupported"
	TurnInterruptNoTurn       TurnInterruptRefusalReason = "no_turn_in_progress"
	TurnInterruptNotStarted   TurnInterruptRefusalReason = "turn_not_yet_started"
	TurnInterruptSuperseded   TurnInterruptRefusalReason = "turn_superseded"
	TurnInterruptSessionEnded TurnInterruptRefusalReason = "session_ended"
	TurnInterruptTimeout      TurnInterruptRefusalReason = "interrupt_timeout"
)

// TurnInterruptRefusal means cancellation could not safely target the intended
// submission. A reply caller may deliver its reply normally after NoTurn or
// Superseded; NotStarted and Unsupported need an explicit caller decision.
type TurnInterruptRefusal struct {
	Reason    TurnInterruptRefusalReason `json:"reason"`
	SessionID string                     `json:"session_id"`
	TurnID    string                     `json:"turn_id,omitempty"`
}

func (e *TurnInterruptRefusal) Error() string {
	return fmt.Sprintf("turn interrupt %s: session %q turn %q", e.Reason, e.SessionID, e.TurnID)
}

// Unwrap preserves the shared runtime error for errors.Is callers while the
// refusal retains the session and turn details for reply surfaces.
func (e *TurnInterruptRefusal) Unwrap() error {
	if e.Reason == TurnInterruptUnsupported {
		return agentsessions.ErrInterruptUnsupported
	}
	if e.Reason == TurnInterruptSessionEnded {
		return agentsessions.ErrSessionNotRunning
	}
	if e.Reason == TurnInterruptTimeout {
		return context.DeadlineExceeded
	}
	return nil
}
