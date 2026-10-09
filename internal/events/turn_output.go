package events

import (
	"github.com/hollis-labs/substrate/harness/adapters/turnoutput"
	"unicode/utf8"
)

// TurnOutputEvent is the durable session.turn_output payload. Text is only an
// excerpt when no message was staged; a message_id references the full output
// that the router attaches to its channel without storing a second copy.
type TurnOutputEvent struct {
	OutputID          string                `json:"output_id,omitempty"`
	FreshConversation bool                  `json:"fresh_conversation,omitempty"`
	ProviderResultID  string                `json:"provider_result_id,omitempty"`
	SessionID         string                `json:"session_id"`
	TurnID            string                `json:"turn_id"`
	Kind              turnoutput.Kind       `json:"kind"`
	StopReason        string                `json:"stop_reason"`
	Confidence        turnoutput.Confidence `json:"confidence"`
	Runtime           string                `json:"runtime"`
	LogicalAgentID    string                `json:"logical_agent_id"`
	ProjectID         string                `json:"project_id"`
	WorkstreamID      string                `json:"workstream_id"`
	MessageID         string                `json:"message_id,omitempty"`
	Text              string                `json:"text,omitempty"`
	TextTruncated     bool                  `json:"text_truncated,omitempty"`
}

// TurnOutputExcerpt is the shared unrouted retention policy for producers and
// hosted receipt verification. It never cuts through a UTF-8 code point.
func TurnOutputExcerpt(text string) (string, bool) {
	const maxBytes = 4 * 1024
	if len(text) <= maxBytes {
		return text, false
	}
	end := maxBytes
	for end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	return text[:end], true
}
