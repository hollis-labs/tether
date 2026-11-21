package teamhost

import (
	"context"
	"database/sql"
	"errors"
	"regexp"

	"github.com/hollis-labs/libs/mesh/core/teams"
	"github.com/hollis-labs/tether/internal/store"
)

var correlationID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// PrepareLaunchCorrelation seals immutable run metadata before eager members
// are provisioned. The workflow ID is deterministic even before run creation.
func (h *Host) PrepareLaunchCorrelation(ctx context.Context, key, task, workstream string) error {
	if key == "" || (task != "" && !correlationID.MatchString(task)) || (workstream != "" && !correlationID.MatchString(workstream) && !store.IsRegistryWorkstreamID(workstream)) {
		return ErrInvalidRequest
	}
	return h.tx(ctx, func(conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, `INSERT INTO team_run_correlation(run_id,launch_key,task_id,workstream_id) VALUES(?,?,?,?) ON CONFLICT DO NOTHING`, id("workflow", key), key, task, workstream)
		if err != nil {
			return err
		}
		var oldTask, oldWorkstream string
		if err := conn.QueryRowContext(ctx, `SELECT task_id,workstream_id FROM team_run_correlation WHERE launch_key=?`, key).Scan(&oldTask, &oldWorkstream); err != nil {
			return err
		}
		if oldTask != task || oldWorkstream != workstream {
			return teams.ErrConflict
		}
		return nil
	})
}

// SessionCorrelation reads the same durable run metadata for eager members and
// later joins. Missing metadata preserves pre-correlation launch behavior.
func SessionCorrelation(ctx context.Context, db *sql.DB, req teams.ProvisionRequest) (run, task, workstream string, err error) {
	if req.RunID != "" {
		err = db.QueryRowContext(ctx, `SELECT run_id,task_id,workstream_id FROM team_run_correlation WHERE run_id=?`, req.RunID).Scan(&run, &task, &workstream)
	} else {
		err = db.QueryRowContext(ctx, `SELECT c.run_id,c.task_id,c.workstream_id FROM team_run_correlation c JOIN team_host_launch_intents i USING(launch_key) WHERE i.intent_key=?`, req.IdempotencyKey).Scan(&run, &task, &workstream)
	}
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return
}
