package teamstore

import (
	"context"
	"database/sql"

	"github.com/hollis-labs/libs/mesh/core/teams"
)

// Prefer the current nonempty roster. Only when it is absent/empty may the
// latest nonempty retained snapshot prove completed teardown. Historical active
// members therefore cannot be hidden by deleting the current roster.
const dissolvedRosterPayloadSQL = `COALESCE(
 (SELECT payload FROM team_rosters WHERE run_id=r.run_id AND json_array_length(payload,'$.members')>0),
 (SELECT payload FROM team_roster_snapshots WHERE run_id=r.run_id AND json_array_length(payload,'$.members')>0 ORDER BY version DESC LIMIT 1))`

const dissolvedRosterProofSQL = `COALESCE(json_array_length(` + dissolvedRosterPayloadSQL + `,'$.members')>0 AND NOT EXISTS(
 SELECT 1 FROM json_each(` + dissolvedRosterPayloadSQL + `,'$.members') m WHERE COALESCE(json_extract(m.value,'$.status'),'') NOT IN ('stopped','released','failed')),0)`

// DissolvedRoster is a read-only recovery view, never a live roster replacement.
// Removed routing and a fully terminal retained roster must both exist.
func (s *Store) DissolvedRoster(ctx context.Context, id string) (teams.Roster, error) {
	var payload []byte
	err := s.db.QueryRowContext(ctx, `SELECT `+dissolvedRosterPayloadSQL+` FROM team_runs r JOIN team_host_routing h ON h.run_id=r.run_id
 WHERE r.run_id=? AND h.removed=1 AND `+dissolvedRosterProofSQL, id).Scan(&payload)
	if err != nil {
		return teams.Roster{}, readError("dissolved roster", err)
	}
	var roster teams.Roster
	err = decode(payload, &roster)
	return roster, err
}

// ReconcileDissolvedRun repairs a run whose teardown committed its removed
// routing and terminal roster but did not commit the run status. Missing/empty
// current rosters require a retained terminal snapshot; live/pending/unknown
// member states are retained.
// The same write fence as roster mutation prevents a concurrent member add
// from racing the proof. JSON patching preserves result and extension fields.
func (s *Store) ReconcileDissolvedRun(ctx context.Context, id string) (settled bool, err error) {
	err = s.fencedImmediate(ctx, func(conn *sql.Conn) error {
		var payload []byte
		if err := conn.QueryRowContext(ctx, `SELECT payload FROM team_runs WHERE run_id=?`, id).Scan(&payload); err != nil {
			return readError("team run", err)
		}
		var run teams.TeamRun
		if err := decode(payload, &run); err != nil {
			return err
		}
		if RunEnded(run.Status) {
			settled = true
			return nil
		}
		var proof bool
		if err := conn.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM team_runs r JOIN team_host_routing h ON h.run_id=r.run_id WHERE r.run_id=? AND h.removed=1 AND `+dissolvedRosterProofSQL+`)`, id).Scan(&proof); err != nil {
			return err
		}

		if !proof {
			return nil
		}
		if _, err := conn.ExecContext(ctx, `UPDATE team_runs SET payload=json_set(payload,'$.status',?) WHERE run_id=?`, string(RunDissolved), id); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `UPDATE team_run_deadlines SET closed=1 WHERE run_id=?`, id); err != nil {
			return err
		}
		settled = true
		return nil
	})
	return settled, err
}

// ReconcileDissolvedRuns selects only removed-routing candidates. Live runs do
// not consume the bounded batch and starve historical dissolved runs.
func (s *Store) ReconcileDissolvedRuns(ctx context.Context, limit int) error {
	if limit < 1 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `SELECT r.run_id FROM team_runs r JOIN team_host_routing h ON h.run_id=r.run_id
 WHERE h.removed=1 AND `+dissolvedRosterProofSQL+` AND json_extract(r.payload,'$.status') NOT IN ('dissolved','completed','failed','canceled','rejected') ORDER BY r.run_id LIMIT ?`, limit)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := s.ReconcileDissolvedRun(ctx, id); err != nil {
			return err
		}
	}
	return nil
}
