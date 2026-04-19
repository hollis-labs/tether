package events

import "time"

// Persister is the storage sink the Bus writes each published event
// to. Implementations live outside this package (internal/store) to
// avoid an events→store import cycle — store already imports events
// for the Scope type.
//
// InsertEvent assigns a monotonic id (used as the Event.Seq) and
// returns the server-side timestamp the row was recorded with; the
// bus copies these onto the Event before fanning out to subscribers,
// guaranteeing persisted-before-delivered ordering.
//
// EventsSince returns all events with id > sinceSeq, in ascending id
// order. It is called at Subscribe time when Filter.SinceSeq > 0, to
// replay history before live delivery begins.
type Persister interface {
	InsertEvent(scope Scope, sessionID, kind, payloadJSON string) (seq int64, at time.Time, err error)
	EventsSince(sinceSeq int64) ([]Event, error)
}
