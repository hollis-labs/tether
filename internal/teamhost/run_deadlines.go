package teamhost

import (
	"context"
	"errors"
	"time"
)

// RunDeadline is one open run in the lifecycle reaper's view. EndReason is
// non-empty when the run has been asked to end early.
type RunDeadline struct {
	RunID     string
	Deadline  time.Time
	EndReason string
}

const deadlineLayout = time.RFC3339Nano

// RecordRunDeadline stores the hard-stop time of a run. The first write wins so
// a replayed formation cannot extend a run's life.
func (h *Host) RecordRunDeadline(ctx context.Context, runID string, deadline time.Time) error {
	if runID == "" || deadline.IsZero() {
		return errors.New("run deadline: run id and deadline required")
	}
	_, err := h.db.ExecContext(ctx, `INSERT OR IGNORE INTO team_run_deadlines(run_id,deadline,recorded_at) VALUES(?,?,?)`,
		runID, deadline.UTC().Format(deadlineLayout), h.now().UTC().Format(deadlineLayout))
	return err
}

// RequestRunEnd asks the reaper to dissolve the run on its next pass. The first
// reason wins.
func (h *Host) RequestRunEnd(ctx context.Context, runID, reason string) error {
	if runID == "" || reason == "" {
		return errors.New("run end: run id and reason required")
	}
	_, err := h.db.ExecContext(ctx, `UPDATE team_run_deadlines SET end_reason=? WHERE run_id=? AND end_reason='' AND closed=0`, reason, runID)
	return err
}

// OpenRuns lists runs that have not been closed, soonest deadline first.
func (h *Host) OpenRuns(ctx context.Context, limit int) ([]RunDeadline, error) {
	rows, err := h.db.QueryContext(ctx, `SELECT run_id,deadline,end_reason FROM team_run_deadlines WHERE closed=0 ORDER BY deadline,run_id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []RunDeadline
	for rows.Next() {
		var d RunDeadline
		var raw string
		if err = rows.Scan(&d.RunID, &raw, &d.EndReason); err != nil {
			return nil, err
		}
		if d.Deadline, err = time.Parse(deadlineLayout, raw); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// CloseRun removes a dissolved run from the reaper's view.
func (h *Host) CloseRun(ctx context.Context, runID string) error {
	_, err := h.db.ExecContext(ctx, `UPDATE team_run_deadlines SET closed=1 WHERE run_id=?`, runID)
	return err
}
