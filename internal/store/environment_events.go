package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/hollis-labs/tether/internal/events"
)

// EnvironmentWindow is one consistent read of the environment-wide cursor.
// A session filter is applied AFTER this read: filtered views never renumber.
type EnvironmentWindow struct {
	HighWater         int64
	EarliestAvailable int64
	Events            []events.Event
	GapReason         string
}

// environmentBounds retains the allocated high-water even after every event
// has been purged. MAX(id) alone would silently rewind that cursor.
func environmentBounds(ctx context.Context, tx *sql.Tx) (head, earliest int64, err error) {
	err = tx.QueryRowContext(ctx, `SELECT MAX(COALESCE((SELECT seq FROM sqlite_sequence WHERE name='events'),0), COALESCE(MAX(id),0)), COALESCE(MIN(id),0) FROM events`).Scan(&head, &earliest)
	if earliest == 0 {
		earliest = head + 1
	}
	return
}

// EnvironmentEvents reads a bounded durable tail. Missing ids (including
// interior retention holes), an ahead cursor, and an oversized catch-up are
// explicit gaps; callers must snapshot instead of accepting a partial tail.
func (s *Store) EnvironmentEvents(ctx context.Context, after int64, limit int) (EnvironmentWindow, error) {
	if after < 0 || limit < 1 || limit > 10000 {
		return EnvironmentWindow{}, fmt.Errorf("invalid environment event window")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return EnvironmentWindow{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var w EnvironmentWindow
	w.HighWater, w.EarliestAvailable, err = environmentBounds(ctx, tx)
	if err != nil {
		return w, err
	}
	if after > w.HighWater {
		w.GapReason = "ahead"
		return w, nil
	}
	if after == w.HighWater {
		return w, nil
	}
	if after < w.EarliestAvailable-1 {
		w.GapReason = "purged"
		return w, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+eventReadColumns+` FROM events WHERE id>? AND id<=? ORDER BY id LIMIT ?`, after, w.HighWater, limit+1)
	if err != nil {
		return w, err
	}
	w.Events, err = scanEvents(rows)
	closeErr := rows.Close()
	if err != nil {
		return w, err
	}
	if closeErr != nil {
		return w, closeErr
	}
	next := after + 1
	for _, e := range w.Events {
		if e.Seq != next {
			w.GapReason = "purged"
			w.Events = nil
			return w, nil
		}
		next++
	}
	if len(w.Events) > limit {
		w.GapReason = "too_large"
		w.Events = nil
		return w, nil
	}
	if next != w.HighWater+1 {
		w.GapReason = "purged"
		w.Events = nil
	}
	return w, nil
}

// EnvironmentSessionRow contains the shell projection's durable baseline.
// Workspace/provider configuration is deliberately not part of this surface.
type EnvironmentSessionRow struct {
	ID, LogicalAgentID, ProviderKind, State, UpdatedAt, RouteJSON string
	LastEventAt                                                   string
}

type EnvironmentSnapshot struct {
	HighWater         int64
	EarliestAvailable int64
	Sessions          []EnvironmentSessionRow
	Requests          []EnvironmentRequest
	// WorkStates is the latest durable work-state per session id; a session
	// that never had one has no entry (unknown, not idle).
	WorkStates map[string]EnvironmentWorkState
}

// EnvironmentWorkState is a session's runtime work-state as last recorded by a
// session.work_state_changed event: "idle" or "processing", and when it began.
type EnvironmentWorkState struct {
	Value string
	At    time.Time
}

// latestWorkStateSQL reads the newest work-state payload of one session. It is an
// index lookup on idx_events_session_kind_at (session_id, kind, at): no sort.
const latestWorkStateSQL = `SELECT payload_json FROM events WHERE session_id=? AND kind=? ORDER BY at DESC, id DESC LIMIT 1`

// environmentWorkStatesSQL reads it for every requested session in one statement.
const environmentWorkStatesSQL = `SELECT s.id, COALESCE((SELECT payload_json FROM events WHERE session_id=s.id AND kind=? ORDER BY at DESC, id DESC LIMIT 1),'') FROM sessions s WHERE (?='' OR s.id=?) ORDER BY s.id`

// parseWorkState reads {"to":..., "at":...}. A payload that does not name a known
// value is no work-state.
func parseWorkState(payload string) (EnvironmentWorkState, bool) {
	var p struct {
		To string `json:"to"`
		At string `json:"at"`
	}
	if payload == "" || json.Unmarshal([]byte(payload), &p) != nil || (p.To != "idle" && p.To != "processing") {
		return EnvironmentWorkState{}, false
	}
	at, _ := time.Parse(time.RFC3339Nano, p.At)
	return EnvironmentWorkState{Value: p.To, At: at}, true
}

// LatestWorkState returns the last recorded work-state of a session.
func (s *Store) LatestWorkState(ctx context.Context, sessionID string) (EnvironmentWorkState, bool, error) {
	var payload string
	err := s.db.QueryRowContext(ctx, latestWorkStateSQL, sessionID, events.KindSessionWorkStateChanged).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return EnvironmentWorkState{}, false, nil
	}
	if err != nil {
		return EnvironmentWorkState{}, false, err
	}
	w, ok := parseWorkState(payload)
	return w, ok, nil
}

// environmentSessionsSQL is the snapshot's session query; a test asserts its
// plan, so the text lives in one place.
const environmentSessionsSQL = `SELECT id,logical_agent_id,provider_kind,state,updated_at,COALESCE(route_json,''),COALESCE((SELECT MAX(at) FROM events WHERE session_id=sessions.id),'') FROM sessions WHERE (?='' OR id=?) ORDER BY id`

// EnvironmentSnapshot reads rows and their cursor in the same SQLite read
// transaction. Lifecycle state writes and subsequent event publication remain
// separate existing operations; a reducer must make state assignments idempotent.
// The last-event time of a session is MAX(at) over idx_events_session
// (session_id, at): a min/max lookup of a few pages. The earlier "ORDER BY id
// DESC LIMIT 1" form could not use that ordering (both session indexes are
// ordered by time, not id), so SQLite read and sorted every event of every
// session on each call: seconds on a cold cache with ~600 sessions and over a
// million events (CW-20261021-0002). The two forms name the same row unless a
// session has an event stamped earlier than one inserted before it; then MAX(at)
// is the later time, which is what "last activity" means. On a copy of the
// production database all 579 rows were identical.
// EnvironmentSnapshot reads rows and their cursor in the same SQLite read
// transaction. Lifecycle state writes and subsequent event publication remain
// separate existing operations; a reducer must make state assignments idempotent.
// The last-event time of a session is MAX(at) over idx_events_session
// (session_id, at): a min/max lookup of a few pages. The earlier "ORDER BY id
// DESC LIMIT 1" form could not use that ordering (both session indexes are
// ordered by time, not id), so SQLite read and sorted every event of every
// session on each call: seconds on a cold cache with ~600 sessions and over a
// million events (CW-20261021-0002). Events are written with a monotonic
// time, so the newest id and the newest time name the same row.
func (s *Store) EnvironmentSnapshot(ctx context.Context, sessionID string) (EnvironmentSnapshot, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return EnvironmentSnapshot{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var snap EnvironmentSnapshot
	snap.HighWater, snap.EarliestAvailable, err = environmentBounds(ctx, tx)
	if err != nil {
		return snap, err
	}
	rows, err := tx.QueryContext(ctx, environmentSessionsSQL, sessionID, sessionID)
	if err != nil {
		return snap, err
	}
	defer rows.Close()
	for rows.Next() {
		var row EnvironmentSessionRow
		if err = rows.Scan(&row.ID, &row.LogicalAgentID, &row.ProviderKind, &row.State, &row.UpdatedAt, &row.RouteJSON, &row.LastEventAt); err != nil {
			return snap, err
		}
		snap.Sessions = append(snap.Sessions, row)
	}
	if err = rows.Err(); err != nil {
		return snap, err
	}
	if err = rows.Close(); err != nil {
		return snap, err
	}
	if snap.Requests, err = environmentRequests(ctx, tx, snap.HighWater, sessionID); err != nil {
		return snap, err
	}
	snap.WorkStates, err = environmentWorkStates(ctx, tx, sessionID)
	return snap, err
}

func environmentWorkStates(ctx context.Context, tx *sql.Tx, sessionID string) (map[string]EnvironmentWorkState, error) {
	rows, err := tx.QueryContext(ctx, environmentWorkStatesSQL, events.KindSessionWorkStateChanged, sessionID, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]EnvironmentWorkState{}
	for rows.Next() {
		var id, payload string
		if err := rows.Scan(&id, &payload); err != nil {
			return nil, err
		}
		if w, ok := parseWorkState(payload); ok {
			out[id] = w
		}
	}
	return out, rows.Err()
}
