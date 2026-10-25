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

// environmentBoundsSQL reads the newest and oldest event id. MIN and MAX each sit in
// their own scalar subquery on purpose: SQLite answers a lone MIN(id) or MAX(id) from
// the end of the rowid b-tree in O(log n), but a SELECT holding BOTH (or one combined
// with another expression over the same table) loses that shortcut and scans the whole
// events table. On the hub (1.8M events, 2.7 GB) the combined form took 92 ms warm and
// seconds cold on every snapshot, events read and tether_session_health call
// (CW-20261024-0055). The scan is asserted away by TestEventBoundsQueriesDoNotScanEvents.
const environmentBoundsSQL = `SELECT MAX(COALESCE((SELECT seq FROM sqlite_sequence WHERE name='events'),0), COALESCE((SELECT MAX(id) FROM events),0)), COALESCE((SELECT MIN(id) FROM events),0)`

// environmentBounds retains the allocated high-water even after every event
// has been purged. MAX(id) alone would silently rewind that cursor.
func environmentBounds(ctx context.Context, tx *sql.Tx) (head, earliest int64, err error) {
	err = tx.QueryRowContext(ctx, environmentBoundsSQL).Scan(&head, &earliest)
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

// environmentWorkStateSQL / environmentWorkStatesSQL read the current work-state
// from environment_work_state (migration 0096), a primary-key row maintained in
// the same transaction as the event, so event retention cannot erase it.
const environmentWorkStateSQL = `SELECT value, since FROM environment_work_state WHERE session_id=?`
const environmentWorkStatesSQL = `SELECT session_id, value, since FROM environment_work_state WHERE (?='' OR session_id=?) ORDER BY session_id`

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

// LatestWorkState returns the current work-state of a session, if one was ever recorded.
func (s *Store) LatestWorkState(ctx context.Context, sessionID string) (EnvironmentWorkState, bool, error) {
	var value, since string
	err := s.db.QueryRowContext(ctx, environmentWorkStateSQL, sessionID).Scan(&value, &since)
	if errors.Is(err, sql.ErrNoRows) {
		return EnvironmentWorkState{}, false, nil
	}
	if err != nil {
		return EnvironmentWorkState{}, false, err
	}
	at, _ := time.Parse(time.RFC3339Nano, since)
	return EnvironmentWorkState{Value: value, At: at}, true, nil
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
	rows, err := tx.QueryContext(ctx, environmentWorkStatesSQL, sessionID, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]EnvironmentWorkState{}
	for rows.Next() {
		var id, value, since string
		if err := rows.Scan(&id, &value, &since); err != nil {
			return nil, err
		}
		at, _ := time.Parse(time.RFC3339Nano, since)
		out[id] = EnvironmentWorkState{Value: value, At: at}
	}
	return out, rows.Err()
}
