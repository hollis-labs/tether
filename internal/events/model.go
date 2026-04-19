// Package events defines the Event type, scope constants, and the
// pub/sub bus that publishes lifecycle and broker events to
// subscribers while persisting them to the store. The bus arrives in
// Sprint v002-06; v0.0.2 storage primitives (scope, nullable
// session_id, payload_json) land in Sprint v002-03.
package events

import "time"

// Scope classifies an event's origin. Session events reference a
// specific runtime session; daemon events describe muxd lifecycle;
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
// LogicalAgentID is carried for observability on lifecycle events but
// is not persisted as a column in v0.0.2; emitters that want it to
// survive replay should embed it in PayloadJSON.
type Event struct {
	Seq            int64
	At             time.Time
	Scope          Scope
	SessionID      string
	LogicalAgentID string
	Kind           string
	PayloadJSON    string
}
