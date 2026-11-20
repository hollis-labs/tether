package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/hollis-labs/tether/internal/launch"
)

// ErrSessionCorrelationConflict refuses replacement of an existing stamp or
// addition of a missing stamp once execution has begun.
var ErrSessionCorrelationConflict = errors.New("session correlation conflict")

// FillSessionLaunchCorrelation atomically fills absent row/plan stamps. The
// caller must hold the canonical session launch gate and admit the operator.
// The create idempotency request/digest remains untouched: a replay describes
// the original create, while the same session retains its launch-time stamps.
func (s *Store) FillSessionLaunchCorrelation(ctx context.Context, id, task, workstream string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var state string
	var oldTask, oldWorkstream sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT state,task_id,workstream_id FROM sessions WHERE id=?`, id).Scan(&state, &oldTask, &oldWorkstream)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrSessionNotFound
	}
	if err != nil {
		return err
	}
	if (task != "" && oldTask.String != "" && task != oldTask.String) || (workstream != "" && oldWorkstream.String != "" && workstream != oldWorkstream.String) {
		return fmt.Errorf("%w: a nonempty task_id/workstream_id cannot be replaced", ErrSessionCorrelationConflict)
	}
	fillTask, fillWorkstream := task != "" && oldTask.String == "", workstream != "" && oldWorkstream.String == ""
	if !fillTask && !fillWorkstream {
		return nil
	}
	if state != "created" {
		return fmt.Errorf("%w: stamps may only be filled before launch", ErrSessionCorrelationConflict)
	}
	if !fillTask {
		task = oldTask.String
	}
	if !fillWorkstream {
		workstream = oldWorkstream.String
	}
	var raw string
	if err = tx.QueryRowContext(ctx, `SELECT plan_json FROM launch_plans WHERE session_id=?`, id).Scan(&raw); err != nil {
		return err
	}
	var plan launch.Plan
	if err = json.Unmarshal([]byte(raw), &plan); err != nil {
		return err
	}
	plan.TaskID, plan.WorkstreamID = task, workstream
	// Preserve the other correlation dimensions and unrelated environment values.
	if plan.Env == nil {
		plan.Env = map[string]string{}
	}
	for key, value := range map[string]string{"TETHER_TASK_ID": task, "TETHER_WORKSTREAM_ID": workstream} {
		delete(plan.Env, key)
		if value != "" {
			plan.Env[key] = value
		}
	}
	encoded, err := json.Marshal(&plan)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE sessions SET task_id=?,workstream_id=?,updated_at=? WHERE id=?`, sql.NullString{String: task, Valid: task != ""}, sql.NullString{String: workstream, Valid: workstream != ""}, time.Now().UTC().Format(time.RFC3339Nano), id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE launch_plans SET plan_json=? WHERE session_id=?`, string(encoded), id); err != nil {
		return err
	}
	return tx.Commit()
}
