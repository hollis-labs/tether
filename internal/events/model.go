// Package events defines the Event type, scope constants, and the
// pub/sub bus that publishes lifecycle and broker events to
// subscribers while persisting them to the store. The bus arrives in
// Sprint v002-06; v0.0.2 storage primitives (scope, nullable
// session_id, payload_json) land in Sprint v002-03.
package events

import "time"

// Scope classifies an event's origin. Session events reference a
// specific runtime session; daemon events describe tetherd lifecycle;
// broker events describe envelope delivery. The set is open-ended —
// callers may introduce new scopes as needed; the types here are
// conveniences, not an enum gate.
type Scope = string

const (
	ScopeSession Scope = "session"
	ScopeDaemon  Scope = "daemon"
	ScopeBroker  Scope = "broker"
)

// Event is the in-memory shape exchanged by the bus. Seq is the
// monotonic stream id assigned by the persister (the events table's
// AUTOINCREMENT primary key); it is 0 on events not yet persisted.
// SessionID is empty for daemon/broker events (column is nullable).
// LogicalAgentID is persisted as the logical-agent scope dimension and
// hydrated on replay. Envelope.Dims.LogicalAgentID takes precedence if supplied.
type Event struct {
	// EventID is the producer-minted global id (D6: evt_ + 32 hex), assigned and
	// persisted when the row is written. Empty on rows that predate it. Fan-out is
	// at least once and carries the same EventID on a repeat. Until subscribers
	// dedupe on it, Seq is the dedupe key (a paged subscriber already skips any
	// Seq at or below its cursor; SubscribeLive consumers may see repeats).
	EventID        string
	Seq            int64
	At             time.Time
	Scope          Scope
	SessionID      string
	LogicalAgentID string
	// Envelope carries the persisted v1.1 blocks (scope dims, class, corr, actor,
	// ...). Zero means none: nothing is stamped by the bus itself.
	Envelope    Envelope
	Kind        string
	PayloadJSON string
}
