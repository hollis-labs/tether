package teamhost

import (
	"context"
	"database/sql"
	"errors"

	"github.com/hollis-labs/substrate/mesh"
	"github.com/hollis-labs/substrate/mesh/teams"
)

func (h *Host) GetRun(ctx context.Context, runID string) (teams.TeamRun, error) {
	result, err := h.store.GetRun(ctx, runID)
	return result.Run, err
}
func (h *Host) LaunchWorkflow(ctx context.Context, key string, definition teams.WorkflowDefinition) (string, error) {
	if key == "" || definition.TeamID == "" || definition.TeamVersion == 0 {
		return "", errors.New("workflow: key and definition required")
	}
	authored, err := h.store.GetDefinition(ctx, definition.TeamID, definition.TeamVersion)
	if err != nil {
		return "", err
	}
	if authored.Authority.Mode != teams.Strict {
		return "", teams.ErrDenied
	}
	expected, err := teams.CompileTeam(authored, authored.Phases)
	if err != nil {
		return "", err
	}
	expectedPayload, err := encode(expected)
	if err != nil {
		return "", err
	}
	equal, err := same(expectedPayload, definition)
	if err != nil {
		return "", err
	}
	if !equal {
		return "", teams.ErrConflict
	}
	runID := id("workflow", key)
	payload, err := encode(definition)
	if err != nil {
		return "", err
	}
	var failed bool
	err = h.db.QueryRowContext(ctx, `SELECT failed FROM team_host_workflows WHERE launch_key=?`, key).Scan(&failed)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if failed {
		return "", teams.ErrLaunchFailed
	}
	run := teams.TeamRun{ID: runID, TeamID: definition.TeamID, TeamVersion: definition.TeamVersion, Status: mesh.TaskWorking, Channel: "team/" + runID}
	_, err = h.store.CreateRunAtomic(ctx, run, func(conn *sql.Conn) error {
		var old []byte
		var failed bool
		err := conn.QueryRowContext(ctx, `SELECT definition,failed FROM team_host_workflows WHERE launch_key=?`, key).Scan(&old, &failed)
		if err == nil {
			if failed {
				return teams.ErrLaunchFailed
			}
			equal, err := same(old, definition)
			if err != nil {
				return err
			}
			if !equal {
				return teams.ErrConflict
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		_, err = conn.ExecContext(ctx, `INSERT INTO team_host_workflows(launch_key,definition,run_id) VALUES(?,?,?)`, key, payload, runID)
		return err
	})
	if err != nil {
		return "", err
	}
	return runID, nil
}
func (h *Host) FailWorkflow(ctx context.Context, key, reason string) error {
	if key == "" {
		return errors.New("workflow failure: key required")
	}
	return h.tx(ctx, func(conn *sql.Conn) error {
		runID := id("workflow", key)
		_, err := conn.ExecContext(ctx, `INSERT INTO team_host_workflows(launch_key,run_id,failed,failure) VALUES(?,?,1,?) ON CONFLICT(launch_key) DO UPDATE SET failed=1,failure=CASE WHEN failed=0 THEN excluded.failure ELSE failure END`, key, runID, reason)
		if err != nil {
			return err
		}
		var payload []byte
		err = conn.QueryRowContext(ctx, `SELECT payload FROM team_runs WHERE run_id=?`, runID).Scan(&payload)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		var run teams.TeamRun
		if err = decode(payload, &run); err != nil {
			return err
		}
		run.Status = mesh.TaskFailed
		payload, err = encode(run)
		if err != nil {
			return err
		}
		_, err = conn.ExecContext(ctx, `UPDATE team_runs SET payload=? WHERE run_id=?`, payload, runID)
		return err
	})
}
