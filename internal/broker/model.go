// Package broker defines the BrokerEnvelope type used for
// inter-session mailbox messaging. v0.0.2 ships the model + storage
// primitives only (see internal/store/broker_envelopes.go);
// request/reply semantics and routing arrive in Sprint v003-05.
//
// Envelope.ID is expected to be a UUIDv7 string (sortable by creation
// time) per critical-state note #9 on the active boot prompt. The
// storage layer treats it as opaque TEXT.
package broker

// Envelope mirrors the broker_envelopes row. Timestamps are RFC3339
// strings; empty string ↔ SQL NULL for nullable columns. Priority is
// an int with application-defined semantics (typical: 0 = default).
type Envelope struct {
	ID            string
	Sender        string
	Recipient     string
	WorkflowID    string
	CorrelationID string
	MessageType   string
	Priority      int
	Payload       string
	CreatedAt     string
	DeliveredAt   string
	ConsumedAt    string
	AuditJSON     string
}
