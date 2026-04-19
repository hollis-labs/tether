// Package events defines the Event type and scope constants for the
// evolving events stream. v0.0.2 ships the types only — the pub/sub
// bus and `/events/stream` endpoint arrive in Sprint v002-06. Callers
// write events via store.LogEvent today; Sprint 6 will add a bus that
// fan-outs writes to subscribers.
package events

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

// Event mirrors the events row. SessionID may be empty (column is
// nullable); scope is always set.
type Event struct {
	ID          int64
	Scope       Scope
	SessionID   string
	At          string
	Kind        string
	PayloadJSON string
}
