package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// EventConsumerCursor returns the last event sequence the named in-daemon
// consumer finished with, and whether it has one (CW-20261121-0154).
func (s *Store) EventConsumerCursor(ctx context.Context, consumer string) (int64, bool, error) {
	var seq int64
	err := s.db.QueryRowContext(ctx, `SELECT sequence FROM event_consumer_cursors WHERE consumer=?`, consumer).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return seq, true, nil
}

// AdvanceEventConsumerCursor records the consumer's position. It never rewinds.
func (s *Store) AdvanceEventConsumerCursor(ctx context.Context, consumer string, seq int64) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO event_consumer_cursors(consumer,sequence,updated_at) VALUES(?,?,?)
ON CONFLICT(consumer) DO UPDATE SET sequence=MAX(sequence,excluded.sequence), updated_at=excluded.updated_at`,
		consumer, seq, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

// OpsStatusNudged reports whether the session was already reminded of kind for task.
func (s *Store) OpsStatusNudged(ctx context.Context, sessionID, taskID, kind string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM ops_status_nudges WHERE session_id=? AND task_id=? AND kind=?`, sessionID, taskID, kind).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// MarkOpsStatusNudged records a reminder that was delivered.
func (s *Store) MarkOpsStatusNudged(ctx context.Context, sessionID, taskID, kind string) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO ops_status_nudges(session_id,task_id,kind,nudged_at) VALUES(?,?,?,?)`,
		sessionID, taskID, kind, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}
