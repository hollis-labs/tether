package store

import (
	"context"
	"database/sql"
	"encoding/json"
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

func (s *Store) ReaperSessions(ctx context.Context) ([]ReaperSession, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,COALESCE(pid_started_at,'') FROM sessions WHERE state IN ('launching','running','detached') ORDER BY created_at`)
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
		var started sql.NullString
		err = s.db.QueryRowContext(ctx, `SELECT MIN(at) FROM events WHERE session_id=? AND kind='session.state_changed' AND json_valid(payload_json) AND json_extract(payload_json,'$.to') IN ('launching','running')`, row.ID).Scan(&started)
		if err != nil {
			return nil, err
		}
		if started.Valid {
			ids[i].StartedAt, _ = time.Parse(time.RFC3339Nano, started.String)
		}

		var at sql.NullString
		err = s.db.QueryRowContext(ctx, `SELECT MAX(CASE WHEN kind='session.activity' THEN json_extract(payload_json,'$.at') ELSE at END) FROM events WHERE session_id=? AND kind IN ('session.activity','session.turn_output','session.turn_routed','tool_call_start','tool_call_end','provider.permission_denied')`, row.ID).Scan(&at)
		if err != nil {
			return nil, err
		}
		if at.Valid {
			ids[i].LastActivity, _ = time.Parse(time.RFC3339Nano, at.String)
		}
	}
	return ids, nil
}

// RecordReaperOrphan commits authority revocation and a visible outcome together.
// A late natural completion wins: active-state predicates never overwrite it.
func (s *Store) RecordReaperOrphan(ctx context.Context, id, reason string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	at := time.Now().UTC()
	stamp := at.Format(time.RFC3339Nano)
	res, err := tx.ExecContext(ctx, `UPDATE sessions SET state='orphaned',pid=NULL,pid_started_at=NULL,updated_at=? WHERE id=? AND state IN ('launching','running','detached')`, stamp, id)
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
	if _, err = tx.ExecContext(ctx, `INSERT INTO events(scope,session_id,at,kind,payload_json) VALUES(?,?,?,?,?)`, events.ScopeSession, id, stamp, "session.reaper", string(payload)); err != nil {
		return false, err
	}
	return true, tx.Commit()
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
	_, _, err = s.InsertEventContext(ctx, events.ScopeSession, id, "session.activity", string(payload))
	return err
}
