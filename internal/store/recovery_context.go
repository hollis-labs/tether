package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/hollis-labs/substrate/mesh/teams"
)

// RecoveryRosters reads existing run/roster facts; it never acquires membership,
// enrolls an actor, changes a binding, or claims a delivery.
func (s *Store) RecoveryRosters(ctx context.Context, agentID string) ([]teams.Roster, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT r.payload,t.payload FROM team_rosters r JOIN team_runs t USING(run_id) ORDER BY r.run_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []teams.Roster
	for rows.Next() {
		var rosterRaw, runRaw []byte
		if err := rows.Scan(&rosterRaw, &runRaw); err != nil {
			return nil, err
		}
		var roster teams.Roster
		var run teams.TeamRun
		if err := json.Unmarshal(runRaw, &run); err != nil {
			return nil, err
		}
		if run.Status.Terminal() {
			continue
		}
		if err := json.Unmarshal(rosterRaw, &roster); err != nil {
			return nil, err
		}
		for _, m := range roster.Members {
			if m.AgentID == agentID && m.Status == "active" && m.Enrolled {
				out = append(out, roster)
				break
			}
		}
	}
	return out, rows.Err()
}

func (s *Store) RecoveryChannelCursor(ctx context.Context, agentID, channel string) (int64, error) {
	var seq int64
	err := s.db.QueryRowContext(ctx, `SELECT sequence FROM agent_recovery_cursors WHERE agent_id=? AND channel=?`, agentID, channel).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return seq, err
}

// AdvanceRecoveryCursors records accepted context delivery, not channel
// consumption. Concurrent recovery can only advance a cursor, never rewind it.
func (s *Store) AdvanceRecoveryCursors(ctx context.Context, agentID string, cursors map[string]int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for name, seq := range cursors {
		if _, err := tx.ExecContext(ctx, `INSERT INTO agent_recovery_cursors(agent_id,channel,sequence) VALUES(?,?,?) ON CONFLICT(agent_id,channel) DO UPDATE SET sequence=MAX(sequence,excluded.sequence)`, agentID, name, seq); err != nil {
			return err
		}
	}
	return tx.Commit()
}
