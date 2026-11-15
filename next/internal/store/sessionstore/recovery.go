package sessionstore

import (
	"context"
	"database/sql"
	"errors"
	"sort"

	gostore "github.com/hollis-labs/libs/core/store"

	"github.com/hollis-labs/tether/next/internal/store"
)

// RecoveryRosterPayloads is one team roster with its run, as stored (both payloads verbatim).
type RecoveryRosterPayloads struct {
	RunID           string
	Roster, TeamRun []byte
}

// RecoveryRosterPayloads returns every roster with its run, ordered by run id. Decoding them,
// skipping ended runs and picking the rosters where the agent is an active enrolled member (the old
// RecoveryRosters) are the team slice's.
func (s *Store) RecoveryRosterPayloads(ctx context.Context) ([]RecoveryRosterPayloads, error) {
	r := s.c.Reader()
	rows, err := r.QueryContext(ctx, `SELECT r.run_id, r.payload, t.payload FROM team_rosters r JOIN team_runs t ON t.run_id = r.run_id ORDER BY r.run_id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []RecoveryRosterPayloads
	for rows.Next() {
		var p RecoveryRosterPayloads
		if err := rows.Scan(&p.RunID, &p.Roster, &p.TeamRun); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// RecoveryChannelCursor returns the agent's recovery cursor on a channel (0 when none).
func (s *Store) RecoveryChannelCursor(ctx context.Context, agentID, channel string) (int64, error) {
	r := s.c.Reader()
	var seq int64
	err := r.QueryRowContext(ctx, rebind(r, `SELECT sequence FROM agent_recovery_cursors WHERE agent_id = ? AND channel = ?`), agentID, channel).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return seq, err
}

// AdvanceRecoveryCursors moves the agent's cursors forward, never back (GREATEST on Postgres, the
// scalar MAX on SQLite), in one transaction. Channels are written in sorted order so concurrent
// advances lock the rows in the same order on Postgres.
func (s *Store) AdvanceRecoveryCursors(ctx context.Context, agentID string, cursors map[string]int64) error {
	names := make([]string, 0, len(cursors))
	for name := range cursors {
		names = append(names, name)
	}
	sort.Strings(names)
	return s.c.WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
		greatest := `MAX(agent_recovery_cursors.sequence, excluded.sequence)`
		if tx.Dialect().Name() == gostore.DriverPostgres {
			greatest = `GREATEST(agent_recovery_cursors.sequence, excluded.sequence)`
		}
		for _, name := range names {
			if _, err := tx.ExecContext(ctx, rebind(tx, `INSERT INTO agent_recovery_cursors (agent_id, channel, sequence) VALUES (?, ?, ?)
 ON CONFLICT (agent_id, channel) DO UPDATE SET sequence = `+greatest), agentID, name, cursors[name]); err != nil {
				return err
			}
		}
		return nil
	})
}
