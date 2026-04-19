package store

import (
	"fmt"
	"time"

	"github.com/chrispian/agent-mux/internal/events"
)

// InsertEvent persists an event row and returns its assigned id
// (used as the bus Event.Seq) and the server-stamped timestamp.
// scope is required (typically one of events.ScopeSession /
// ScopeDaemon / ScopeBroker). sessionID and payloadJSON may be
// empty; both columns store SQL NULL when so. Timestamp format is
// RFC3339 nanoseconds in UTC.
func (s *Store) InsertEvent(scope events.Scope, sessionID, kind, payloadJSON string) (int64, time.Time, error) {
	if scope == "" {
		return 0, time.Time{}, fmt.Errorf("event scope required")
	}
	at := time.Now().UTC()
	res, err := s.db.Exec(
		`INSERT INTO events (scope, session_id, at, kind, payload_json) VALUES (?, ?, ?, ?, ?)`,
		scope, nullIfEmpty(sessionID), at.Format(time.RFC3339Nano),
		kind, nullIfEmpty(payloadJSON),
	)
	if err != nil {
		return 0, time.Time{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, time.Time{}, err
	}
	return id, at, nil
}

// EventsSince returns all events with id > sinceSeq in ascending id
// order. Used by the bus to replay history before live delivery.
// sinceSeq = 0 returns the entire events table.
func (s *Store) EventsSince(sinceSeq int64) ([]events.Event, error) {
	rows, err := s.db.Query(
		`SELECT id, scope, session_id, at, kind, payload_json FROM events WHERE id > ? ORDER BY id ASC`,
		sinceSeq,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []events.Event
	for rows.Next() {
		var (
			id           int64
			scope        string
			sessionID    *string
			atStr        string
			kind         string
			payloadJSON  *string
		)
		if err := rows.Scan(&id, &scope, &sessionID, &atStr, &kind, &payloadJSON); err != nil {
			return nil, err
		}
		at, perr := time.Parse(time.RFC3339Nano, atStr)
		if perr != nil {
			// Fall back to RFC3339 in case earlier rows were written at
			// second precision (pre-InsertEvent v0.0.2 shape).
			at, _ = time.Parse(time.RFC3339, atStr)
		}
		ev := events.Event{
			Seq:   id,
			At:    at,
			Scope: scope,
			Kind:  kind,
		}
		if sessionID != nil {
			ev.SessionID = *sessionID
		}
		if payloadJSON != nil {
			ev.PayloadJSON = *payloadJSON
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}
