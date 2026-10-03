package teamstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"

	"github.com/hollis-labs/substrate/mesh/teams"
)

func (s *Store) Snapshot(ctx context.Context, id string) (teams.Roster, error) {
	var payload []byte
	err := s.db.QueryRowContext(ctx, `SELECT payload FROM team_rosters WHERE run_id=?`, id).Scan(&payload)
	if err != nil {
		return teams.Roster{}, readError("team roster", err)
	}
	var roster teams.Roster
	err = decode(payload, &roster)
	return roster, err
}
func (s *Store) SnapshotAt(ctx context.Context, id string, version uint64) (teams.Roster, error) {
	var payload []byte
	err := s.db.QueryRowContext(ctx, `SELECT payload FROM team_roster_snapshots WHERE run_id=? AND version=?`, id, version).Scan(&payload)
	if err != nil {
		return teams.Roster{}, readError("roster revision", err)
	}
	var roster teams.Roster
	err = decode(payload, &roster)
	return roster, err
}

// Mutate commits one new version, even for a no-op, and retains every snapshot.
// Returns ErrNotFound until CreateRun has created the container.
// The callback must not call this database or perform external side effects.
func (s *Store) Mutate(ctx context.Context, id string, fn func(*teams.Roster) error) error {
	if id == "" || fn == nil {
		return errors.New("roster mutation: run and callback required")
	}
	return s.fencedImmediate(ctx, func(conn *sql.Conn) error {
		if err := requireRun(ctx, conn, id); err != nil {
			return err
		}
		var payload []byte
		err := conn.QueryRowContext(ctx, `SELECT payload FROM team_rosters WHERE run_id=?`, id).Scan(&payload)
		roster := teams.Roster{RunID: id}
		if err == nil {
			if err = decode(payload, &roster); err != nil {
				return err
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("read roster for mutation: %w", err)
		}
		prior := roster.Version
		if prior >= math.MaxInt64 {
			return fmt.Errorf("roster version exhausted: %w", teams.ErrConflict)
		}
		if err = fn(&roster); err != nil {
			return err
		}
		if roster.RunID != id || roster.Version != prior {
			return fmt.Errorf("roster identity/version modified: %w", teams.ErrConflict)
		}
		roster.Version = prior + 1
		payload, err = encode(roster)
		if err != nil {
			return err
		}
		if prior == 0 {
			if _, err = conn.ExecContext(ctx, `INSERT INTO team_rosters(run_id,version,payload) VALUES(?,?,?)`, id, roster.Version, payload); err != nil {
				return fmt.Errorf("create roster: %w", err)
			}
		} else {
			result, err := conn.ExecContext(ctx, `UPDATE team_rosters SET version=?,payload=? WHERE run_id=? AND version=?`, roster.Version, payload, id, prior)
			if err != nil {
				return fmt.Errorf("update roster: %w", err)
			}
			n, err := result.RowsAffected()
			if err != nil {
				return fmt.Errorf("roster affected rows: %w", err)
			}
			if n != 1 {
				return teams.ErrConflict
			}
		}
		if _, err = conn.ExecContext(ctx, `INSERT INTO team_roster_snapshots(run_id,version,payload) VALUES(?,?,?)`, id, roster.Version, payload); err != nil {
			return fmt.Errorf("retain roster snapshot: %w", err)
		}
		return nil
	})
}
