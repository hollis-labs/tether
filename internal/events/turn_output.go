package events

import "github.com/hollis-labs/go-agent-wrapper/turnoutput"

// TurnOutputEvent is the durable session.turn_output payload. Text is only an
// excerpt when no message was staged; a message_id references the full output
// that the router attaches to its channel without storing a second copy.
type TurnOutputEvent struct {
	SessionID      string                `json:"session_id"`
	TurnID         string                `json:"turn_id"`
	Kind           turnoutput.Kind       `json:"kind"`
	StopReason     string                `json:"stop_reason"`
	Confidence     turnoutput.Confidence `json:"confidence"`
	Runtime        string                `json:"runtime"`
	LogicalAgentID string                `json:"logical_agent_id"`
	ProjectID      string                `json:"project_id"`
	WorkstreamID   string                `json:"workstream_id"`
	MessageID      string                `json:"message_id,omitempty"`
	Text           string                `json:"text,omitempty"`
	TextTruncated  bool                  `json:"text_truncated,omitempty"`
}
