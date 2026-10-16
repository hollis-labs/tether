package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/hollis-labs/tether/internal/events"
)

// EventFilter narrows durable queries over the shared events table.
// Results are returned newest first.
type EventFilter struct {
	Scopes    []events.Scope
	Kinds     []string
	SessionID string
	SinceSeq  int64
	Cursor    int64
	Limit     int
}

// InsertEvent persists an event row and returns its assigned id
// (used as the bus Event.Seq) and the server-stamped timestamp.
// scope is required (typically one of events.ScopeSession /
// ScopeDaemon / ScopeBroker). sessionID and payloadJSON may be
// empty; both columns store SQL NULL when so. Timestamp format is
// RFC3339 nanoseconds in UTC.
func (s *Store) InsertEvent(scope events.Scope, sessionID, kind, payloadJSON string) (int64, time.Time, error) {
	return s.InsertEventContext(context.Background(), scope, sessionID, kind, payloadJSON)
}

func (s *Store) InsertEventContext(ctx context.Context, scope events.Scope, sessionID, kind, payloadJSON string) (int64, time.Time, error) {
	seq, at, _, err := s.InsertEventIDContext(ctx, scope, sessionID, kind, payloadJSON)
	return seq, at, err
}

// InsertEventIDContext is InsertEventContext that also returns the D6 event id it
// minted and persisted with the row (events.EventIDPersister).
func (s *Store) InsertEventIDContext(ctx context.Context, scope events.Scope, sessionID, kind, payloadJSON string) (int64, time.Time, string, error) {
	if scope == "" {
		return 0, time.Time{}, "", fmt.Errorf("event scope required")
	}
	if kind == EnvironmentRequestKind {
		return s.insertEnvironmentRequest(ctx, scope, sessionID, payloadJSON)
	}
	at := time.Now().UTC()
	seq, eventID, err := insertEventRow(ctx, s.db, scope, sessionID, kind, payloadJSON, at.Format(time.RFC3339Nano), false)
	if err != nil {
		return 0, time.Time{}, "", err
	}
	return seq, at, eventID, nil
}

// DeleteEventsBefore preserves the bounded events-only seam and writes a
// durable receipt through the shared retention primitive.
func (s *Store) DeleteEventsBefore(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
	batch, err := s.DeleteEventHistoryBefore(ctx, "events", cutoff, limit)
	return batch.Removed, err
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
			`SELECT id, scope, session_id, at, kind, payload_json, event_id FROM events WHERE session_id=? AND id < ? ORDER BY id DESC LIMIT ?`,
			sessionID, cursor, limit,
		)
	} else {
		rows, err = s.db.Query(
			`SELECT id, scope, session_id, at, kind, payload_json, event_id FROM events WHERE session_id=? ORDER BY id DESC LIMIT ?`,
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
	return s.MaxEventSeqContext(context.Background())
}

func (s *Store) MaxEventSeqContext(ctx context.Context) (int64, error) {
	var seq int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM events`).Scan(&seq)
	return seq, err
}

// CountEvents returns the total number of persisted events.
func (s *Store) CountEvents() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&n)
	return n, err
}

// EventsSince returns all events with id > sinceSeq in ascending id
// order. Used by the bus to replay history before live delivery.
// sinceSeq = 0 returns the entire events table.
func (s *Store) EventsSince(sinceSeq int64) ([]events.Event, error) {
	return s.EventsSinceContext(context.Background(), sinceSeq)
}

func (s *Store) EventsSinceContext(ctx context.Context, sinceSeq int64) ([]events.Event, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, scope, session_id, at, kind, payload_json, event_id FROM events WHERE id > ? ORDER BY id ASC`,
		sinceSeq,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEvents(rows)
}

// StreamBoundsContext reports the oldest and newest events-table sequence
// (events.PagedPersister).
func (s *Store) StreamBoundsContext(ctx context.Context) (earliest, head int64, err error) {
	var lo, hi sql.NullInt64
	if err = s.db.QueryRowContext(ctx, `SELECT MIN(id), MAX(id) FROM events`).Scan(&lo, &hi); err != nil {
		return 0, 0, err
	}
	return lo.Int64, hi.Int64, nil
}

// EventsPageContext returns one bounded page of the replay (events.PagedPersister).
// The filter is pushed into SQL so a narrow subscriber does not read, or hold
// the single connection for, rows it will discard in memory.
func (s *Store) EventsPageContext(ctx context.Context, f events.Filter, afterSeq, upToSeq int64, limit int) ([]events.Event, error) {
	if limit <= 0 {
		limit = 512
	}
	q := strings.Builder{}
	q.WriteString(`SELECT id, scope, session_id, at, kind, payload_json, event_id FROM events WHERE id > ? AND id <= ?`)
	args := []any{afterSeq, upToSeq}
	if len(f.Scopes) > 0 {
		q.WriteString(` AND scope IN (` + placeholders(len(f.Scopes)) + `)`)
		for _, v := range f.Scopes {
			args = append(args, v)
		}
	}
	if f.SessionID != "" {
		q.WriteString(` AND session_id = ?`)
		args = append(args, f.SessionID)
	}
	if len(f.Kinds) > 0 {
		q.WriteString(` AND kind IN (` + placeholders(len(f.Kinds)) + `)`)
		for _, v := range f.Kinds {
			args = append(args, v)
		}
	}
	q.WriteString(` ORDER BY id ASC LIMIT ?`)
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEventsWithID(rows)
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// ListRecentEvents returns the newest events across all scopes, newest
// first, up to limit (defaulted to 200, capped at 1000). A pure read for
// operator / dashboard views — the events table is otherwise consumed via
// EventsSince (replay) and Subscribe (live).
func (s *Store) ListRecentEvents(limit int) ([]events.Event, error) {
	if limit <= 0 {
		limit = 200
	}
	if limit > 1000 {
		limit = 1000
	}
	rows, err := s.db.Query(
		`SELECT id, scope, session_id, at, kind, payload_json, event_id FROM events ORDER BY id DESC LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEvents(rows)
}

// QueryEvents returns persisted bus events newest first, filtered by scope,
// kind, session id, lower-bound sequence, and pagination cursor when provided.
func (s *Store) QueryEvents(f EventFilter) ([]events.Event, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}

	query := `SELECT id, scope, session_id, at, kind, payload_json, event_id FROM events`
	var (
		where []string
		args  []any
	)
	if f.SessionID != "" {
		where = append(where, "session_id = ?")
		args = append(args, f.SessionID)
	}
	if f.SinceSeq > 0 {
		where = append(where, "id > ?")
		args = append(args, f.SinceSeq)
	}
	if f.Cursor > 0 {
		where = append(where, "id < ?")
		args = append(args, f.Cursor)
	}
	if len(f.Scopes) > 0 {
		placeholders := make([]string, 0, len(f.Scopes))
		for _, scope := range f.Scopes {
			placeholders = append(placeholders, "?")
			args = append(args, scope)
		}
		where = append(where, "scope IN ("+strings.Join(placeholders, ", ")+")")
	}
	if len(f.Kinds) > 0 {
		placeholders := make([]string, 0, len(f.Kinds))
		for _, kind := range f.Kinds {
			placeholders = append(placeholders, "?")
			args = append(args, kind)
		}
		where = append(where, "kind IN ("+strings.Join(placeholders, ", ")+")")
	}
	if len(where) > 0 {
		//nolint:gosec // G202: where holds fixed column predicates with ? placeholders; values are bound via args
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEvents(rows)
}

// scanEvents scans the events columns "id, scope, session_id, at, kind,
// payload_json, event_id" (event_id is NULL on rows that predate migration 0081).
func scanEvents(rows *sql.Rows) ([]events.Event, error) {
	return scanEventsWithID(rows)
}

// HasPublishedProviderResult checks the full durable session history. Comparing
// only the newest event would re-publish older results in a multi-result replay.
func (s *Store) HasPublishedProviderResult(ctx context.Context, id, resultID string) (bool, error) {
	if resultID == "" {
		return false, nil
	}
	var found bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM events WHERE session_id=? AND kind=? AND CASE WHEN json_valid(payload_json) THEN json_extract(payload_json,'$.provider_result_id') END=?)`, id, events.KindSessionTurnOutput, resultID).Scan(&found)
	return found, err
}
