package teamhost

import (
	"context"
	"database/sql"
	"errors"

	"github.com/hollis-labs/libs/mesh/core/teams"
	"github.com/hollis-labs/tether/internal/teamstore"
)

// ReconcileExpiredFormations only aborts expired formations that never committed
// a workflow/run. The library's existing abort path journals each cleanup step,
// fences delayed workflow creation, and releases/retire intents by their keys.
// It never advances a still-live formation or dissolves a committed run.
func (h *Host) ReconcileExpiredFormations(ctx context.Context, limit int) error {
	if limit < 1 || limit > 1024 {
		return errors.New("formation recovery: limit must be 1..1024")
	}
	ledger := orphanLedger{Store: h.store, host: h}
	launcher := teams.Launcher{Definitions: h.store, Roster: h.store, Ledger: ledger, Provisioner: h, Workflows: h, Routing: h, Clock: h, IDs: h}
	report, err := launcher.Reconcile(ctx, "", limit)
	return errors.Join(append(report.Failures, err)...)
}

type orphanLedger struct {
	*teamstore.Store
	host *Host
}

// Rotate a bounded durable cursor over unfinished formations, including those
// with no port receipts or with a terminal port refusal. Live rows cannot starve
// older orphans beyond a page boundary.
func (l orphanLedger) Pending(ctx context.Context, _ string, limit int) ([]string, error) {
	var keys []string
	err := l.host.tx(ctx, func(c *sql.Conn) error {
		var cursor int64
		err := c.QueryRowContext(ctx, `SELECT position FROM team_host_recovery_cursors WHERE kind='formations'`).Scan(&cursor)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		rows, err := c.QueryContext(ctx, `SELECT rowid,launch_key FROM team_launches WHERE state IN ('planning','prepared','aborting') ORDER BY CASE WHEN rowid>? THEN 0 ELSE 1 END,rowid LIMIT ?`, cursor, limit)
		if err != nil {
			return err
		}
		for rows.Next() {
			var key string
			if err = rows.Scan(&cursor, &key); err != nil {
				_ = rows.Close()
				return err
			}
			keys = append(keys, key)
		}
		if err = errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		if len(keys) == 0 {
			return nil
		}
		_, err = c.ExecContext(ctx, `INSERT INTO team_host_recovery_cursors(kind,position) VALUES('formations',?) ON CONFLICT(kind) DO UPDATE SET position=excluded.position`, cursor)
		return err
	})
	return keys, err
}

func (l orphanLedger) WithLease(ctx context.Context, key string, resume func(context.Context) error) error {
	// Avoid contending with healthy formations. Recheck under the lease below;
	// this detached observation never authorizes cleanup.
	candidate, err := l.GetLaunch(ctx, key)
	if err != nil {
		return err
	}
	if !l.expired(candidate) {
		return nil
	}
	// Bound contention with a concurrent request for this expired formation.
	leaseCtx, cancel := context.WithTimeout(ctx, teamstore.RecoveryBackoffCap)
	defer cancel()
	return l.Store.WithLease(leaseCtx, key, func(leased context.Context) error {
		record, err := l.GetLaunch(leased, key)
		if err != nil {
			return err
		}
		if !l.expired(record) {
			return nil
		}
		// A workflow acknowledgement may have been lost before Run reached the
		// launch journal. Refuse cleanup if the independently committed run exists.
		var committed bool
		err = l.host.db.QueryRowContext(leased, `SELECT EXISTS(SELECT 1 FROM team_runs WHERE run_id=?) OR EXISTS(SELECT 1 FROM team_host_workflows w JOIN team_runs r ON r.run_id=w.run_id WHERE w.launch_key=?)`, id("workflow", key), key).Scan(&committed)
		if err != nil || committed {
			return err
		}
		ready, err := l.RecoveryReady(leased, "formation", key, "abort", l.host.Now())
		if err != nil || !ready {
			return err
		}
		err = resume(leased)
		// ErrLaunchFailed is the expected terminal result of a successful abort;
		// cleanup failures leave Aborting and must remain retryable and observable.
		after, readErr := l.GetLaunch(leased, key)
		if readErr == nil && after.State == teams.Failed && errors.Is(err, teams.ErrLaunchFailed) {
			err = nil
		}
		return l.FinishRecovery(leased, "formation", key, "abort", l.host.Now(), errors.Join(err, readErr))
	})
}

func (l orphanLedger) expired(record teams.LaunchRecord) bool {
	return record.Run.ID == "" && !record.Deadline.IsZero() && !l.host.Now().Before(record.Deadline) &&
		(record.State == teams.Planning || record.State == teams.Prepared || record.State == teams.Aborting)
}
