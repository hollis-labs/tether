package store

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/hollis-labs/tether/internal/events"
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

// ListEventsBySession returns events bound to a single session in
// descending id order (newest first), up to limit. cursor restricts
// the result to id < cursor; pass 0 on the first page. Limit is
// clamped to [1, 1000] — callers pass -1 or 0 for the default (100).
//
// Used by GET /sessions/{id}/events for operator UI / debugging. The
// Sprint 6 Bus already covers live + full-replay needs via EventsSince
// + Subscribe; this method exists for the narrower per-session
// historical view.
func (s *Store) ListEventsBySession(sessionID string, limit int, cursor int64) ([]events.Event, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	var (
		rows *sql.Rows
		err  error
	)
	if cursor > 0 {
		rows, err = s.db.Query(
			`SELECT id, scope, session_id, at, kind, payload_json FROM events WHERE session_id=? AND id < ? ORDER BY id DESC LIMIT ?`,
			sessionID, cursor, limit,
		)
	} else {
		rows, err = s.db.Query(
			`SELECT id, scope, session_id, at, kind, payload_json FROM events WHERE session_id=? ORDER BY id DESC LIMIT ?`,
			sessionID, limit,
		)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEvents(rows)
}

// MaxEventSeq returns the highest event sequence number (id) currently
// in the events table, or 0 if the table is empty. Used by the MCP proxy
// forwarder to subscribe live-only without replaying history.
func (s *Store) MaxEventSeq() (int64, error) {
	var seq int64
	err := s.db.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM events`).Scan(&seq)
	return seq, err
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
	return scanEvents(rows)
}

// scanEvents is shared by EventsSince + ListEventsBySession. Row shape
// is identical; only ORDER BY differs.
func scanEvents(rows *sql.Rows) ([]events.Event, error) {
	var out []events.Event
	for rows.Next() {
		var (
			id          int64
			scope       string
			sessionID   *string
			atStr       string
			kind        string
			payloadJSON *string
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
