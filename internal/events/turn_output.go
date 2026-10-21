package events

import (
	"fmt"
	"github.com/hollis-labs/libs/harness/core/adapters/turnoutput"
	"unicode/utf8"
)

// TurnOutputEvent is the durable session.turn_output payload. Text is only an
// excerpt when no message was staged; a message_id references the full output
// that the router attaches to its channel without storing a second copy.
type TurnOutputEvent struct {
	// Reason and Size describe policy-floor withholding; Text stays absent.
	Reason            string                `json:"reason,omitempty"`
	Size              int                   `json:"size,omitempty"`
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
	TaskID            string                `json:"task_id,omitempty"`
	ProjectID         string                `json:"project_id"`
	WorkstreamID      string                `json:"workstream_id"`
	MessageID         string                `json:"message_id,omitempty"`
	Text              string                `json:"text,omitempty"`
	TextTruncated     bool                  `json:"text_truncated,omitempty"`
	// Audience records who asked for the turn (CW-20261021-0010): operator, agent,
	// system or unknown, with the evidence class in AudienceBasis. InputClasses is
	// filled only when the input was bound by proven order. Absent on outputs that
	// predate it, which reads as unknown. No sender identity is ever copied here.
	Audience      string   `json:"audience,omitempty"`
	AudienceBasis string   `json:"audience_basis,omitempty"`
	InputIDs      []string `json:"input_ids,omitempty"`
	InputClasses  []string `json:"input_classes,omitempty"`
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

// SecretWithheldNotice is the app-authored replacement for a policy-floor
// secret reply. It contains only the durable session/turn identity and size.
func SecretWithheldNotice(sessionID, turnID string, size int) string {
	return fmt.Sprintf("reply withheld: secret (session=%s turn=%s size=%d bytes)", sessionID, turnID, size)
}
