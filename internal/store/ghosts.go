package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/hollis-labs/tether/internal/events"
)

// Ghost cleanup (CW-20261114-0007): rows that claim a session, a shim or a worker is alive when it is
// not. The app classifies them against live state (worker inventories, process probes); these are the
// reads and the guarded writes it needs. Every write re-checks the row it was shown, so a session that
// came back to life between the report and the apply is left alone.

// OpenSession is a session row that claims to be alive.
type OpenSession struct {
	ID, WorkerID, State, LaunchID, LogicalAgentID, CreatedAt, UpdatedAt, PIDStartedAt string
	PID                                                                               int64
}

// openStates are the session states that claim a live process. created/ready rows never launched.
const openStates = `('launching','running','detached','idle','orphaned')`

// OpenSessions lists every session row in an open state.
func (s *Store) OpenSessions(ctx context.Context) ([]OpenSession, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,COALESCE(worker_id,''),state,launch_id,logical_agent_id,created_at,updated_at,COALESCE(pid_started_at,''),COALESCE(pid,0) FROM sessions WHERE state IN `+openStates+` ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []OpenSession
	for rows.Next() {
		var o OpenSession
		if err := rows.Scan(&o.ID, &o.WorkerID, &o.State, &o.LaunchID, &o.LogicalAgentID, &o.CreatedAt, &o.UpdatedAt, &o.PIDStartedAt, &o.PID); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// ActiveBindings counts the unrevoked runtime bindings of a session.
func (s *Store) ActiveBindings(ctx context.Context, sessionID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM runtime_bindings WHERE session_id=? AND revoked_at IS NULL`, sessionID).Scan(&n)
	return n, err
}

// CloseGhostSession ends a ghost session as failed, revoking its session credential and runtime
// bindings and publishing a session.reaper outcome, all in one transaction. It changes nothing (false)
// unless the row is still in the state and at the updated_at it was classified at.
func (s *Store) CloseGhostSession(ctx context.Context, row OpenSession, reason string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	at := time.Now().UTC()
	stamp := at.Format(time.RFC3339Nano)
	res, err := tx.ExecContext(ctx, `UPDATE sessions SET state='failed',pid=NULL,pid_started_at=NULL,ended_at=COALESCE(ended_at,?),updated_at=? WHERE id=? AND state=? AND updated_at=?`, stamp, stamp, row.ID, row.State, row.UpdatedAt)
	if err != nil {
		return false, err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE principals SET revoked_at=COALESCE(revoked_at,?) WHERE kind='session' AND session_id=?`, stamp, row.ID); err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE runtime_bindings SET revoked_at=?,updated_at=? WHERE session_id=? AND revoked_at IS NULL`, stamp, stamp, row.ID); err != nil {
		return false, err
	}
	payload, _ := json.Marshal(map[string]string{"reason": reason, "stage": "outcome", "outcome": "failed", "from": row.State})
	if err = s.publishOutboxEvent(ctx, tx, events.Event{Scope: events.ScopeSession, SessionID: row.ID, Kind: "session.reaper", PayloadJSON: string(payload), At: at}); err != nil {
		return false, err
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	s.EventsCommitted()
	return true, nil
}

// ShimCustody is a hub-local shim custody row (session_shims) of an ended session.
type ShimCustody struct {
	SessionID, State, SocketPath  string
	HostPID, ShimPID, ProviderPID int64
	// Frozen custody belongs to a replaced session; its history is retained by trigger.
	Frozen bool
}

// EndedShimCustody lists the session_shims rows whose session has ended.
func (s *Store) EndedShimCustody(ctx context.Context) ([]ShimCustody, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT x.session_id,s.state,x.socket_path,x.host_pid,x.shim_pid,x.provider_pid,
		EXISTS(SELECT 1 FROM session_replacements r WHERE r.source_session_id=x.session_id)
		FROM session_shims x JOIN sessions s ON s.id=x.session_id WHERE s.state IN ('completed','failed','killed') ORDER BY x.session_id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ShimCustody
	for rows.Next() {
		var c ShimCustody
		if err := rows.Scan(&c.SessionID, &c.State, &c.SocketPath, &c.HostPID, &c.ShimPID, &c.ProviderPID, &c.Frozen); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// DeleteShimCustody removes the shim custody row of an ended session. It changes nothing (false) when the
// session is no longer ended or the custody is frozen by a replacement.
func (s *Store) DeleteShimCustody(ctx context.Context, sessionID string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM session_shims WHERE session_id=?
		AND EXISTS(SELECT 1 FROM sessions s WHERE s.id=session_shims.session_id AND s.state IN ('completed','failed','killed'))
		AND NOT EXISTS(SELECT 1 FROM session_replacements r WHERE r.source_session_id=session_shims.session_id)`, sessionID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// StaleWorker is a worker row not seen since a cutoff, with what still references it.
type StaleWorker struct {
	ID, Label, LastSeen string
	Sessions, Open      int
	Grants              int
	// SupersededBy is the newest other unretired row with the same label seen after this one, if any.
	SupersededBy string
}

// StaleWorkers lists unretired worker rows last seen before cutoff.
func (s *Store) StaleWorkers(ctx context.Context, cutoff time.Time) ([]StaleWorker, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT w.id,w.label,w.last_seen,
		(SELECT COUNT(*) FROM sessions s WHERE s.worker_id=w.id),
		(SELECT COUNT(*) FROM sessions s WHERE s.worker_id=w.id AND s.state IN `+openStates+`),
		(SELECT COUNT(*) FROM worker_mail_grants g WHERE g.worker_id=w.id),
		COALESCE((SELECT n.id FROM workers n WHERE n.label=w.label AND n.id<>w.id AND n.retired_at IS NULL AND n.last_seen>w.last_seen ORDER BY n.last_seen DESC LIMIT 1),'')
		FROM workers w WHERE w.retired_at IS NULL AND w.last_seen < ? ORDER BY w.last_seen`, cutoff.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []StaleWorker
	for rows.Next() {
		var w StaleWorker
		if err := rows.Scan(&w.ID, &w.Label, &w.LastSeen, &w.Sessions, &w.Open, &w.Grants, &w.SupersededBy); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// RetireWorker hides a stale worker row from listings and label lookups, recording what superseded it.
// It changes nothing (false) when the worker was seen again or holds an open session.
func (s *Store) RetireWorker(ctx context.Context, id, supersededBy, lastSeen string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE workers SET retired_at=?,superseded_by=NULLIF(?,'') WHERE id=? AND retired_at IS NULL AND last_seen=?
		AND NOT EXISTS(SELECT 1 FROM sessions s WHERE s.worker_id=workers.id AND s.state IN `+openStates+`)`,
		time.Now().UTC().Format(time.RFC3339Nano), supersededBy, id, lastSeen)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}
