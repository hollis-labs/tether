package store

import (
	"context"
	"time"
)

// RecordedExitCandidates selects inconsistent running rows with an observed
// exit. PID zero or absence from a process-local manager alone is not evidence.
// Worker rows are included only when their durable exit has already arrived.
func (s *Store) RecordedExitCandidates(ctx context.Context, after string, limit int) ([]string, error) {
	if limit < 1 || limit > 64 {
		limit = 64
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM sessions WHERE id>? AND state='running' AND COALESCE(pid,0)=0 AND exit_code IS NOT NULL AND COALESCE(ended_at,'')<>'' AND NOT EXISTS(SELECT 1 FROM session_shims WHERE session_id=sessions.id) ORDER BY id LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ReconcileRecordedExit changes only the inconsistent state. Recheck the entire
// snapshot in the write so a late launch/worker update wins over this scan.
// Outcome fields (including ended_at and exit code) are retained.
func (s *Store) ReconcileRecordedExit(ctx context.Context, row SessionRow) (bool, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE sessions SET state=CASE WHEN exit_code=0 THEN 'completed' ELSE 'failed' END, updated_at=?
 WHERE id=? AND state='running' AND state=? AND COALESCE(pid,0)=0 AND pid IS ? AND exit_code IS ? AND ended_at IS ? AND COALESCE(ended_at,'')<>'' AND exit_code IS NOT NULL AND worker_id=? AND updated_at=?`,
		time.Now().UTC().Format(time.RFC3339Nano), row.ID, row.State, row.PID, row.ExitCode, row.EndedAt, row.WorkerID, row.UpdatedAt)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n > 0, err
}
