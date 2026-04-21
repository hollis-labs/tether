package broker

// MessageType is the enum of valid broker envelope message types.
// See ADR 0018 for the correlation-ID scheme and envelope lifecycle.
type MessageType = string

const (
	// TypeRequest initiates a request/reply exchange. The server assigns
	// a UUIDv7 correlation_id; the requester waits for a matching response.
	TypeRequest = "request"

	// TypeResponse carries the reply to a request. Must include a
	// correlation_id matching the originating request envelope's ID.
	TypeResponse = "response"

	// TypeNotice is a one-way informational message. No reply expected.
	TypeNotice = "notice"

	// TypeEscalation signals that human (or higher-priority agent) attention
	// is needed. Routing to a human queue is deferred to v0.1.
	TypeEscalation = "escalation"

	// TypeHandoff signals a cross-logical-agent session transfer.
	// Distinct from the same-agent handoff event in Sprint v004-04.
	TypeHandoff = "handoff"

	// TypeStatusUpdate carries a periodic agent status report.
	TypeStatusUpdate = "status_update"
)

// validTypes is the canonical allow-list for envelope message_type validation.
var validTypes = map[string]bool{
	TypeRequest:      true,
	TypeResponse:     true,
	TypeNotice:       true,
	TypeEscalation:   true,
	TypeHandoff:      true,
	TypeStatusUpdate: true,
}

// IsValidMessageType reports whether mt is a known envelope message type.
func IsValidMessageType(mt string) bool {
	return validTypes[mt]
}

// ValidMessageTypes returns the full list of valid message type strings.
// Stable ordering for tests and documentation.
func ValidMessageTypes() []string {
	return []string{
		TypeRequest,
		TypeResponse,
		TypeNotice,
		TypeEscalation,
		TypeHandoff,
		TypeStatusUpdate,
	}
}
