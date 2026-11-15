package sessionstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/hollis-labs/tether/next/internal/store"
	"github.com/hollis-labs/tether/next/internal/store/eventstore"
)

// EventStamp is the event slice's enrichment of an outbox row before it is written (the old
// publishOutboxEvent stamped the session's dimensions). nil writes the row as the store built it.
type EventStamp func(eventstore.Row) eventstore.Row

func (f EventStamp) apply(r eventstore.Row) eventstore.Row {
	if f == nil {
		return r
	}
	return f(r)
}

// ReaperSession is a snapshot only: mutations re-check the durable state.
type ReaperSession struct {
	SessionRow
	PIDStartedAt string
	LastActivity time.Time
	StartedAt    time.Time
}

// reaperActivityKinds are the only event kinds that count as genuine runtime activity. Noise kinds
// (session.output, session.shim_status, session.state_changed re-asserts) never do: a dead session
// whose recovery bookkeeping keeps writing diagnostics must not look alive.
var reaperActivityKinds = []string{"session.activity", "session.turn_output", "session.turn_routed", "tool_call_start", "tool_call_end", "provider.permission_denied"}

// ReaperCandidates lists the hub-local sessions the reaper may act on (worker_id empty, live state),
// oldest first, with StartedAt from created_at; it reads no events (ReaperTimes does, lazily). The
// old store listed the ids and then read each row on its own; this is one query, so the rows are one
// snapshot. Ties on created_at break by id.
func (s *Store) ReaperCandidates(ctx context.Context) ([]ReaperSession, error) {
	r := s.c.Reader()
	rows, err := r.QueryContext(ctx, `SELECT `+sessionColumns+`, COALESCE(pid_started_at,'') FROM sessions
 WHERE worker_id = '' AND state IN ('launching','running','detached') ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ReaperSession
	for rows.Next() {
		var c ReaperSession
		row, err := scanSession(func(dest ...any) error { return rows.Scan(append(dest, &c.PIDStartedAt)...) })
		if err != nil {
			return nil, err
		}
		c.SessionRow = row
		c.StartedAt, _ = time.Parse(time.RFC3339Nano, row.CreatedAt)
		out = append(out, c)
	}
	return out, rows.Err()
}

// ReaperTimes fills StartedAt (the first session.state_changed to launching or running) and
// LastActivity (the newest activity-kind event) from bounded, indexed lookups on the class B log
// (idx_events_session_kind_at): it never aggregates over a session's whole history.
func (s *Store) ReaperTimes(ctx context.Context, r *ReaperSession) error {
	b := s.all.B().Reader()
	rows, err := b.QueryContext(ctx, rebind(b, `SELECT at, payload_json FROM events WHERE session_id = ? AND kind = 'session.state_changed' ORDER BY at ASC, id ASC LIMIT 256`), r.ID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var at string
		var payload sql.NullString
		if err := rows.Scan(&at, &payload); err != nil {
			_ = rows.Close()
			return err
		}
		var change struct {
			To string `json:"to"`
		}
		if payload.Valid && json.Unmarshal([]byte(payload.String), &change) == nil && (change.To == StateLaunching || change.To == StateRunning) {
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
		err := b.QueryRowContext(ctx, rebind(b, `SELECT at, payload_json FROM events WHERE session_id = ? AND kind = ? ORDER BY at DESC, id DESC LIMIT 1`), r.ID, kind).Scan(&at, &payload)
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

// ReaperSessions loads every candidate with its times (the daemon sweep uses ReaperCandidates and
// ReaperTimes, so unchanged sessions are skipped without event reads).
func (s *Store) ReaperSessions(ctx context.Context) ([]ReaperSession, error) {
	out, err := s.ReaperCandidates(ctx)
	if err != nil {
		return nil, err
	}
	for i := range out {
		if err := s.ReaperTimes(ctx, &out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// LatestSessionEvent returns the newest event of one kind for a session (by at, then id, through
// idx_events_session_kind_at), or nil when there is none. It never reads the session's other events:
// the row is located here and read through the event store.
func (s *Store) LatestSessionEvent(ctx context.Context, events *eventstore.Store, sessionID, kind string) (*eventstore.Row, error) {
	b := s.all.B().Reader()
	var seq int64
	err := b.QueryRowContext(ctx, rebind(b, `SELECT id FROM events WHERE session_id = ? AND kind = ? ORDER BY at DESC, id DESC LIMIT 1`), sessionID, kind).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rows, err := events.Query(ctx, eventstore.Filter{SessionID: sessionID, Kinds: []string{kind}, SinceSeq: seq - 1, Cursor: seq + 1, Limit: 1})
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return &rows[0], nil
}

// RecordReaperOrphan orphans a live session, revokes its session credentials (A) and bindings, and
// writes the session.reaper outcome as an outbox row (B), all in the allowlisted joint
// "RecordReaperOrphan": the outcome is published only if the orphaning commits. A late natural
// completion wins: the UPDATE is guarded on a live state (and, with observed, on the exact snapshot
// the reaper examined: state, updated_at and pid), so it changes nothing once the session moved on and
// reports false. The caller wakes the outbox after a true result (the old EventsCommitted).
func (s *Store) RecordReaperOrphan(ctx context.Context, events *eventstore.Store, id, reason string, observed *SessionRow, stamp EventStamp) (bool, error) {
	at := s.now()
	ts := at.Format(time.RFC3339Nano)
	query := `UPDATE sessions SET state = 'orphaned', pid = NULL, pid_started_at = NULL, updated_at = ? WHERE id = ? AND state IN ('launching','running','detached')`
	args := []any{ts, id}
	if observed != nil {
		var pid int64
		if observed.PID != nil {
			pid = *observed.PID
		}
		query += ` AND state = ? AND updated_at = ? AND COALESCE(pid, 0) = ?`
		args = append(args, observed.State, observed.UpdatedAt, pid)
	}
	payload, _ := json.Marshal(map[string]string{"reason": reason, "stage": "outcome", "outcome": "orphaned"})
	var orphaned bool
	err := s.all.Joint(ctx, "RecordReaperOrphan", func(ctx context.Context, tx store.Tx) error {
		orphaned = false
		res, err := tx.ExecContext(ctx, rebind(tx, query), args...)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			return err
		}
		for _, q := range []string{
			`UPDATE principals SET revoked_at = COALESCE(revoked_at, ?) WHERE kind = 'session' AND session_id = ?`,
			`UPDATE runtime_bindings SET revoked_at = COALESCE(revoked_at, ?) WHERE session_id = ?`,
		} {
			if _, err := tx.ExecContext(ctx, rebind(tx, q), ts, id); err != nil {
				return err
			}
		}
		row := stamp.apply(eventstore.Row{Scope: "session", SessionID: id, Kind: "session.reaper", PayloadJSON: string(payload), At: at})
		if _, _, err := events.InsertTx(ctx, tx, row, true); err != nil {
			return err
		}
		orphaned = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return orphaned, nil
}

// SessionLeaseExpired reports whether every current binding of the session has an expired lease: it
// ignores superseded and revoked generations, and treats a live binding, or one with no lease bound,
// as authority to continue (false when the session has no current binding). The old query compared
// julianday() values; here the leases are read and compared in Go at the same instant, and an
// unparsable lease counts as not live, as julianday's NULL did.
func (s *Store) SessionLeaseExpired(ctx context.Context, id string, now time.Time) (bool, error) {
	r := s.c.Reader()
	rows, err := r.QueryContext(ctx, rebind(r, `SELECT lease_expires_at FROM runtime_bindings WHERE session_id = ? AND revoked_at IS NULL
 AND generation = (SELECT MAX(b.generation) FROM runtime_bindings b WHERE b.target_urn = runtime_bindings.target_urn AND b.revoked_at IS NULL)`), id)
	if err != nil {
		return false, err
	}
	defer func() { _ = rows.Close() }()
	total, live := 0, 0
	for rows.Next() {
		var lease sql.NullString
		if err := rows.Scan(&lease); err != nil {
			return false, err
		}
		total++
		if !lease.Valid {
			live++
			continue
		}
		if t, ok := repParseJulian(lease.String); ok && t.After(now) {
			live++
		}
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	return total > 0 && live == 0, nil
}

// repParseJulian parses a stored time the way julianday() reads one: RFC 3339 with or without a
// fraction, or without a zone (UTC), with 'T' or a space.
func repParseJulian(v string) (time.Time, bool) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999", "2006-01-02 15:04:05.999999999", "2006-01-02 15:04:05.999999999Z07:00"} {
		if t, err := time.Parse(layout, v); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// RecordWorkerOutput stores a worker's resume cursor for a session (session_worker_seq, C) and the
// session.output event for that frame as an outbox row (B) in the allowlisted joint
// "RecordWorkerOutput": the cursor can never say a frame was handled when its event was lost. The
// caller wakes the outbox after it returns nil.
func (s *Store) RecordWorkerOutput(ctx context.Context, events *eventstore.Store, sessionID string, seq int64, payloadJSON string, at time.Time, stamp EventStamp) error {
	return s.all.Joint(ctx, "RecordWorkerOutput", func(ctx context.Context, tx store.Tx) error {
		if _, err := tx.ExecContext(ctx, rebind(tx, `INSERT INTO session_worker_seq (session_id, last_seq) VALUES (?, ?)
 ON CONFLICT (session_id) DO UPDATE SET last_seq = excluded.last_seq`), sessionID, seq); err != nil {
			return err
		}
		row := stamp.apply(eventstore.Row{Scope: "session", SessionID: sessionID, Kind: "session.output", At: at, PayloadJSON: payloadJSON})
		_, _, err := events.InsertTx(ctx, tx, row, true)
		return err
	})
}
