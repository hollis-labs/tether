package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/hollis-labs/tether/internal/events"
)

// ReaperSession is a snapshot only. Mutations re-check the durable state.
type ReaperSession struct {
	SessionRow
	PIDStartedAt string
	LastActivity time.Time
	StartedAt    time.Time
}

// reaperActivityKinds are the only event kinds that count as genuine runtime activity. Noise kinds
// (session.output, session.shim_status, session.state_changed re-asserts) never do: a dead session whose
// recovery bookkeeping keeps writing diagnostics must not look alive, and must not be re-scanned for them.
var reaperActivityKinds = []string{"session.activity", "session.turn_output", "session.turn_routed", "tool_call_start", "tool_call_end", "provider.permission_denied"}

// ReaperCandidates lists hub-local sessions the reaper may act on, without touching the events table.
// Start and activity times are loaded lazily with ReaperTimes, only for sessions that are actually examined.
func (s *Store) ReaperCandidates(ctx context.Context) ([]ReaperSession, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,COALESCE(pid_started_at,'') FROM sessions WHERE worker_id='' AND state IN ('launching','running','detached') ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	var ids []ReaperSession
	for rows.Next() {
		var r ReaperSession
		if err = rows.Scan(&r.ID, &r.PIDStartedAt); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ids = append(ids, r)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	for i := range ids {
		row, err := s.GetSession(ids[i].ID)
		if err != nil {
			return nil, err
		}
		ids[i].SessionRow = *row
		ids[i].StartedAt, _ = time.Parse(time.RFC3339Nano, row.CreatedAt)
	}
	return ids, nil
}

// ReaperTimes fills StartedAt and LastActivity from indexed, bounded lookups
// (idx_events_session_kind_at). It never aggregates over a session's whole event history and never
// runs a JSON function across many rows: session.state_changed rows per session are few, and each
// activity kind is a single newest-row lookup.
func (s *Store) ReaperTimes(ctx context.Context, r *ReaperSession) error {
	rows, err := s.db.QueryContext(ctx, `SELECT at,payload_json FROM events WHERE session_id=? AND kind='session.state_changed' ORDER BY at ASC,id ASC LIMIT 256`, r.ID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var at string
		var payload sql.NullString
		if err = rows.Scan(&at, &payload); err != nil {
			_ = rows.Close()
			return err
		}
		var change struct {
			To string `json:"to"`
		}
		if payload.Valid && json.Unmarshal([]byte(payload.String), &change) == nil && (change.To == "launching" || change.To == "running") {
			if t, perr := time.Parse(time.RFC3339Nano, at); perr == nil {
				r.StartedAt = t
			}
			break
		}
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	var last time.Time
	for _, kind := range reaperActivityKinds {
		var at string
		var payload sql.NullString
		err = s.db.QueryRowContext(ctx, `SELECT at,payload_json FROM events WHERE session_id=? AND kind=? ORDER BY at DESC,id DESC LIMIT 1`, r.ID, kind).Scan(&at, &payload)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		stamp := at
		if kind == "session.activity" && payload.Valid {
			var body struct {
				At string `json:"at"`
			}
			if json.Unmarshal([]byte(payload.String), &body) == nil && body.At != "" {
				stamp = body.At
			}
		}
		if t, perr := time.Parse(time.RFC3339Nano, stamp); perr == nil && t.After(last) {
			last = t
		}
	}
	r.LastActivity = last
	return nil
}

// ReaperSessions loads every candidate with its times. The daemon sweep uses ReaperCandidates and
// ReaperTimes so unchanged sessions are skipped without any event reads; this form remains for callers
// and tests that want the full picture.
func (s *Store) ReaperSessions(ctx context.Context) ([]ReaperSession, error) {
	ids, err := s.ReaperCandidates(ctx)
	if err != nil {
		return nil, err
	}
	for i := range ids {
		if err := s.ReaperTimes(ctx, &ids[i]); err != nil {
			return nil, err
		}
	}
	return ids, nil
}

// LatestSessionEvent returns the newest event of one kind for a session via idx_events_session_kind_at,
// or nil when there is none. Unlike QueryEvents it never reads the session's other events.
func (s *Store) LatestSessionEvent(ctx context.Context, sessionID, kind string) (*events.Event, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+eventReadColumns+` FROM events WHERE session_id=? AND kind=? ORDER BY at DESC,id DESC LIMIT 1`, sessionID, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out, err := scanEvents(rows)
	if err != nil || len(out) == 0 {
		return nil, err
	}
	return &out[0], nil
}

// RecordReaperOrphan commits authority revocation and a visible outcome together.
// A late natural completion wins: active-state predicates never overwrite it.
func (s *Store) RecordReaperOrphan(ctx context.Context, id, reason string, observed ...SessionRow) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	at := time.Now().UTC()
	stamp := at.Format(time.RFC3339Nano)
	query := `UPDATE sessions SET state='orphaned',pid=NULL,pid_started_at=NULL,updated_at=? WHERE id=? AND state IN ('launching','running','detached')`
	args := []any{stamp, id}
	if len(observed) > 0 {
		row := observed[0]
		query += ` AND state=? AND updated_at=? AND COALESCE(pid,0)=?`
		args = append(args, row.State, row.UpdatedAt, row.PID.Int64)
	}
	res, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil || n == 0 {
		return false, err
	}
	for _, q := range []string{`UPDATE principals SET revoked_at=COALESCE(revoked_at,?) WHERE kind='session' AND session_id=?`, `UPDATE runtime_bindings SET revoked_at=COALESCE(revoked_at,?) WHERE session_id=?`} {
		if _, err = tx.ExecContext(ctx, q, stamp, id); err != nil {
			return false, err
		}
	}
	payload, _ := json.Marshal(map[string]string{"reason": reason, "stage": "outcome", "outcome": "orphaned"})
	// The outcome event is an outbox row on this transaction: it commits with the
	// orphaning, and is published only if that commit happens.
	if err = s.publishOutboxEvent(ctx, tx, events.Event{Scope: events.ScopeSession, SessionID: id, Kind: "session.reaper", PayloadJSON: string(payload), At: at}); err != nil {
		return false, err
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	s.EventsCommitted()
	return true, nil
}

// SessionLeaseExpired ignores superseded/revoked generations and treats a live
// replacement or an unbounded lease as authority to continue.
func (s *Store) SessionLeaseExpired(ctx context.Context, id string, now time.Time) (bool, error) {
	var total, live int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(CASE WHEN lease_expires_at IS NULL OR julianday(lease_expires_at)>julianday(?) THEN 1 ELSE 0 END),0) FROM runtime_bindings WHERE session_id=? AND revoked_at IS NULL AND generation=(SELECT MAX(b.generation) FROM runtime_bindings b WHERE b.target_urn=runtime_bindings.target_urn AND b.revoked_at IS NULL)`, now.UTC().Format(time.RFC3339Nano), id).Scan(&total, &live)
	return total > 0 && live == 0, err
}

// RecordSessionActivity persists only genuine runtime activity observed since
// the previous sweep. Its original timestamp survives daemon restart.
func (s *Store) RecordSessionActivity(ctx context.Context, id string, at time.Time) error {
	payload, err := json.Marshal(map[string]string{"at": at.UTC().Format(time.RFC3339Nano)})
	if err != nil {
		return err
	}
	// Use one caller-owned transaction for enrichment and the staged insert.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	err = s.publishOutboxEvent(ctx, tx, events.Event{Scope: events.ScopeSession, SessionID: id, Kind: "session.activity", PayloadJSON: string(payload)})
	if err == nil {
		err = tx.Commit()
	}
	if err == nil {
		s.EventsCommitted()
	}
	return err
}

// RecordWorkerOutput stores a worker's resume cursor for a session and the
// session.output event for that frame in one transaction: one commit and one sync
// instead of two (CW-20261023-0018), and the cursor can no longer say a frame was
// handled when its event was lost. The event is a pending outbox row; the bus
// publishes it after the commit.
func (s *Store) RecordWorkerOutput(ctx context.Context, sessionID string, seq int64, payloadJSON string, at time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `INSERT INTO session_worker_seq(session_id,last_seq) VALUES(?,?) ON CONFLICT(session_id) DO UPDATE SET last_seq=excluded.last_seq`, sessionID, seq); err != nil {
		return err
	}
	if err = s.publishOutboxEvent(ctx, tx, events.Event{Scope: events.ScopeSession, SessionID: sessionID, Kind: "session.output", At: at, PayloadJSON: payloadJSON}); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	s.EventsCommitted()
	return nil
}
