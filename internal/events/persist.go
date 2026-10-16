package events

import (
	"context"
	"time"
)

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

// ContextPersister bounds production persistence and replay. The older seam is
// retained for in-memory embedders; Tether's SQL store implements this one.
type ContextPersister interface {
	InsertEventContext(context.Context, Scope, string, string, string) (int64, time.Time, error)
	EventsSinceContext(context.Context, int64) ([]Event, error)
}

// PagedPersister bounds replay. The bus pages history outside its publish lock
// instead of loading everything since a sequence into memory, and uses the
// bounds to tell a subscriber when history it asked for was trimmed. Tether's
// SQL store implements it; embedders without it keep the one-shot EventsSince.
type PagedPersister interface {
	// StreamBoundsContext returns the oldest and newest stored sequence
	// (0, 0 when the log is empty).
	StreamBoundsContext(ctx context.Context) (earliest, head int64, err error)
	// EventsPageContext returns up to limit events with afterSeq < Seq <= upToSeq
	// that match f's Scopes, SessionID and Kinds, in ascending Seq order.
	EventsPageContext(ctx context.Context, f Filter, afterSeq, upToSeq int64, limit int) ([]Event, error)
}

// GapPayload is the JSON payload of a KindStreamGap event.
type GapPayload struct {
	// Reason is overrun (live events were dropped for a slow consumer),
	// trimmed (requested history no longer exists), evicted (the subscriber
	// made no progress and was closed) or replay_error.
	Reason string `json:"reason"`
	// Dropped counts live events dropped since the previous signal.
	Dropped int64 `json:"dropped,omitempty"`
	// LastSeq is the last sequence the subscriber processed; resume with
	// since_seq=LastSeq.
	LastSeq int64 `json:"last_seq"`
	// EarliestAvailable and HighWater bound what the log holds, on every gap
	// frame. A persister that cannot page reports EarliestAvailable 0 (unknown)
	// and HighWater as the newest sequence this bus published.
	EarliestAvailable int64 `json:"earliest_available"`
	HighWater         int64 `json:"high_water"`
	// Recovered says the bus re-read the dropped events from the durable log and
	// delivers them next, so nothing was lost.
	Recovered bool `json:"recovered,omitempty"`
}
