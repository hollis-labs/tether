package store

import (
	"context"
	"database/sql"
	"fmt"

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
	rows, err := tx.QueryContext(ctx, `SELECT id,scope,session_id,at,kind,payload_json FROM events WHERE id>? AND id<=? ORDER BY id LIMIT ?`, after, w.HighWater, limit+1)
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
}

// EnvironmentSnapshot reads rows and their cursor in the same SQLite read
// transaction. Lifecycle state writes and subsequent event publication remain
// separate existing operations; a reducer must make state assignments idempotent.
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
	rows, err := tx.QueryContext(ctx, `SELECT id,logical_agent_id,provider_kind,state,updated_at,COALESCE(route_json,''),COALESCE((SELECT at FROM events WHERE session_id=sessions.id ORDER BY id DESC LIMIT 1),'') FROM sessions WHERE (?='' OR id=?) ORDER BY id`, sessionID, sessionID)
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
	snap.Requests, err = environmentRequests(ctx, tx, snap.HighWater, sessionID)
	return snap, err
}
