package events

const (
	KindSessionTurnInterruptRequested = "session.turn_interrupt_requested"
	KindSessionTurnInterruptCompleted = "session.turn_interrupt_completed"
)

// TurnInterruptEvent audits the actor, targeted submission and cancellation
// outcome without including the reply body or provider credentials.
type TurnInterruptEvent struct {
	Actor        string `json:"actor"`
	SessionID    string `json:"session_id"`
	TurnID       string `json:"turn_id,omitempty"`
	OutputTurnID string `json:"output_turn_id,omitempty"`
	Result       string `json:"result"`
	Error        string `json:"error,omitempty"`
}
