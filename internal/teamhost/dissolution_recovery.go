package teamhost

import (
	"context"
	"database/sql"
	"errors"
	"reflect"

	"github.com/hollis-labs/libs/mesh/core/teams"
)

type dissolutionRecoveryKey struct{}

// WithDissolutionRecovery restores a persisted observation-only cleanup fence.
func WithDissolutionRecovery(ctx context.Context) context.Context {
	return context.WithValue(ctx, dissolutionRecoveryKey{}, true)
}

// DissolutionRecovery identifies cleanup that may acknowledge an observed end
// but must never issue a new runtime stop to manufacture recovery evidence.
func DissolutionRecovery(ctx context.Context) bool {
	value, _ := ctx.Value(dissolutionRecoveryKey{}).(bool)
	return value
}

// recoverInterruptedDissolution resumes only the durable all-nonroutable
// teardown plan. No live session is stopped merely to make that proof true.
func (h *Host) recoverInterruptedDissolution(ctx context.Context, id string) (bool, error) {
	run, err := h.GetRun(ctx, id)
	if err != nil {
		return false, err
	}
	definition, err := h.store.GetDefinition(ctx, run.TeamID, run.TeamVersion)
	if err != nil {
		return false, err
	}
	if definition.Authority.Mode != teams.Strict {
		return false, nil
	}
	roster, ready, err := h.store.InterruptedDissolution(ctx, id)
	if errors.Is(err, teams.ErrNotFound) {
		return false, nil
	}
	if err != nil || !ready {
		return false, err
	}
	// Validate the complete plan before issuing even one keyed cleanup effect.
	for _, m := range roster.Members {
		if m.Status != "stopping" && m.Status != "releasing" {
			continue
		}
		ok, err := h.recoveryMemberReady(ctx, id, m)
		if err != nil || !ok {
			return false, err
		}
	}
	for _, m := range roster.Members {
		if m.Status != "stopping" && m.Status != "releasing" {
			continue
		}
		current, err := h.store.Snapshot(ctx, id)
		if err != nil {
			return false, err
		}
		if !reflect.DeepEqual(current, roster) {
			return false, teams.ErrConflict
		}
		ok, err := h.recoveryMemberReady(ctx, id, m)
		if err != nil || !ok {
			return false, err
		}
		mode := "release"
		if m.Resolution == teams.Fresh {
			mode = "retire"
		}
		if err = h.end(WithDissolutionRecovery(ctx), m, mode); err != nil {
			return false, err
		}
	}
	return h.store.FinishInterruptedDissolution(ctx, roster)
}

func (h *Host) recoveryMemberReady(ctx context.Context, id string, m teams.Member) (bool, error) {
	ready, adopted, err := h.store.TerminationReady(ctx, id, m)
	if err != nil || !ready || m.Intent == nil {
		return ready, err
	}
	if adopted {
		return h.ports.Adopter != nil, nil
	}
	sessions, ok := h.ports.Sessions.(interface {
		CanRecoverStop(context.Context, teams.Member) (bool, error)
	})
	if !ok {
		return false, nil
	}
	return sessions.CanRecoverStop(ctx, m)
}

// Rotate past unresolved/live transitions, including across daemon restarts.
func (h *Host) interruptedDissolutionPage(ctx context.Context, limit int) (ids []string, err error) {
	err = h.tx(ctx, func(c *sql.Conn) error {
		var cursor int64
		err := c.QueryRowContext(ctx, `SELECT position FROM team_host_recovery_cursors WHERE kind='dissolutions'`).Scan(&cursor)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		rows, err := c.QueryContext(ctx, `SELECT r.rowid,r.run_id FROM team_runs r JOIN team_rosters t USING(run_id)
WHERE json_extract(r.payload,'$.status') NOT IN ('dissolved','completed','failed','canceled','rejected')
AND EXISTS(SELECT 1 FROM json_each(t.payload,'$.members') m WHERE json_extract(m.value,'$.status') IN ('stopping','releasing'))
AND NOT EXISTS(SELECT 1 FROM json_each(t.payload,'$.members') m WHERE COALESCE(json_extract(m.value,'$.status'),'') NOT IN ('stopped','released','failed','stopping','releasing'))
ORDER BY CASE WHEN r.rowid>? THEN 0 ELSE 1 END,r.rowid LIMIT ?`, cursor, limit)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id string
			if err = rows.Scan(&cursor, &id); err != nil {
				_ = rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		if err = errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		_, err = c.ExecContext(ctx, `INSERT INTO team_host_recovery_cursors(kind,position) VALUES('dissolutions',?) ON CONFLICT(kind) DO UPDATE SET position=excluded.position`, cursor)
		return err
	})
	return
}
