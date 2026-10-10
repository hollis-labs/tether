package teamstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/hollis-labs/substrate/mesh"
	"github.com/hollis-labs/substrate/mesh/teams"
)

// ListRostersForActor reads current snapshots in run-ID order. Membership is
// selected within the same SQL statement as the snapshot, not from an earlier
// caller-supplied list of run IDs. Returned host intents stay internal.
func (s *Store) ListRostersForActor(ctx context.Context, actor mesh.URN, kind mesh.ActorKind, after string, limit int) ([]teams.Roster, error) {
	if (mesh.Actor{URN: actor, Kind: kind}).Validate() != nil || limit < 1 || limit > 101 {
		return nil, errors.New("team roster: invalid selector")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT r.run_id,r.payload FROM team_rosters r
		WHERE r.run_id>? AND EXISTS(SELECT 1 FROM json_each(r.payload,'$.members') m
		WHERE json_extract(m.value,'$.actor')=? AND json_extract(m.value,'$.kind')=?
		AND json_extract(m.value,'$.status') IN ('active','stopped','released','stopping','releasing','failing','failed'))
		ORDER BY r.run_id LIMIT ?`, after, actor, kind, limit)
	if err != nil {
		return nil, fmt.Errorf("list team rosters: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := []teams.Roster{}
	for rows.Next() {
		var id string
		var payload []byte
		var roster teams.Roster
		if err := rows.Scan(&id, &payload); err != nil {
			return nil, fmt.Errorf("read team roster: %w", err)
		}
		if err := decode(payload, &roster); err != nil {
			return nil, err
		}
		if roster.RunID != id {
			return nil, teams.ErrConflict
		}
		result = append(result, roster)
	}
	return result, rows.Err()
}
