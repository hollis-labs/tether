package sessionstore

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	gostore "github.com/hollis-labs/libs/core/store"

	"github.com/hollis-labs/tether/next/internal/store"
)

// teamCommitHold, when set (tests only, export_test.go), runs in CommitGoneTeamShimRecovery after
// every check and before the first write, so a test can hold racers at the point the lock protects.
var teamCommitHold func()

// TeamShimRecoveryFence is a retained custody snapshot plus the session's single retained team
// recovery binding: never a grant. Only the host's confirmed gone-and-retired path consumes it, and
// CommitGoneTeamShimRecovery rechecks every part of it in its own transaction.
type TeamShimRecoveryFence struct {
	Custody SessionShimRow
	TeamRecoveryFence
}

// shimTeamMember is "the launch plan marks a team member" (old json_extract(plan_json,
// '$.team_member')=1). On Postgres the plan is read under the pg_input_is_valid guard, after each
// \u0000 escape is replaced with U+FFFD so a plan SQLite reads is read; true and 1 both count, as
// json_extract's 1 does.
func shimTeamMember(q gostore.Querier) string {
	if q.Dialect().Name() == gostore.DriverPostgres {
		const plan = `replace(l.plan_json, '\u0000', '�')`
		return `COALESCE((CASE WHEN pg_input_is_valid(` + plan + `, 'jsonb') THEN (` + plan + `)::jsonb->'team_member' END) IN ('true'::jsonb, '1'::jsonb), false)`
	}
	return `COALESCE(json_extract(l.plan_json, '$.team_member') = 1, 0)`
}

// GoneTeamShimRecoveryFence returns the fence for a live team member whose stored placement still
// equals custody, or ErrTeamRecoveryUnavailable. It reads one snapshot.
func (s *Store) GoneTeamShimRecoveryFence(ctx context.Context, custody SessionShimRow) (TeamShimRecoveryFence, error) {
	var f TeamShimRecoveryFence
	err := s.c.ReadTx(ctx, func(ctx context.Context, q gostore.Querier) error {
		current, err := SessionShimTx(ctx, q, custody.SessionID)
		if err != nil || current != custody {
			return ErrTeamRecoveryUnavailable
		}
		var eligible bool
		if err := q.QueryRowContext(ctx, rebind(q, `SELECT EXISTS(SELECT 1 FROM sessions s JOIN launch_plans l ON l.session_id = s.id
 WHERE s.id = ? AND s.state IN ('launching','running','detached') AND `+shimTeamMember(q)+`)`), custody.SessionID).Scan(&eligible); err != nil || !eligible {
			return ErrTeamRecoveryUnavailable
		}
		binding, err := s.TeamRecoveryFenceFor(ctx, q, custody.SessionID)
		if err != nil {
			return err
		}
		f = TeamShimRecoveryFence{Custody: current, TeamRecoveryFence: binding}
		return nil
	})
	if err != nil {
		return TeamShimRecoveryFence{}, err
	}
	return f, nil
}

// CommitGoneTeamShimRecovery runs after a matching canonical placement retirement and positive
// host/provider absence. Under the session row lock it requires the stored placement and the
// retained binding to equal expected and the session to be a live team member, then archives the
// secret-free custody (team_gone_shim_recoveries, recovery_pending), releases that exact placement
// row and marks the session orphaned. It never revokes or restores a principal, binding or
// enrollment.
func (s *Store) CommitGoneTeamShimRecovery(ctx context.Context, expected TeamShimRecoveryFence) (SessionStateChange, error) {
	id := expected.Custody.SessionID
	change := SessionStateChange{SessionID: id, To: "orphaned", Reason: "team_shim_recovery_pending"}
	err := s.c.WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
		if err := shimLockSession(ctx, tx, id); err != nil {
			return err
		}
		current, err := SessionShimTx(ctx, tx, id)
		if err != nil || current != expected.Custody {
			return ErrTeamRecoveryUnavailable
		}
		binding, err := s.TeamRecoveryFenceFor(ctx, tx, id)
		if err != nil {
			return err
		}
		if binding != expected.TeamRecoveryFence {
			return ErrTeamRecoveryUnavailable
		}
		var agent *string
		var team bool
		if err := tx.QueryRowContext(ctx, rebind(tx, `SELECT s.state, s.logical_agent_id, `+shimTeamMember(tx)+`
 FROM sessions s JOIN launch_plans l ON l.session_id = s.id WHERE s.id = ?`), id).Scan(&change.From, &agent, &team); err != nil || !team {
			return ErrTeamRecoveryUnavailable
		}
		if agent != nil {
			change.LogicalAgentID = *agent
		}
		switch change.From {
		case "launching", "running", "detached":
		default:
			return ErrTeamRecoveryUnavailable
		}
		if teamCommitHold != nil {
			teamCommitHold()
		}
		raw, err := json.Marshal(current)
		if err != nil {
			return err
		}
		now := s.now().Format(time.RFC3339Nano)
		if _, err := tx.ExecContext(ctx, rebind(tx, `INSERT INTO team_gone_shim_recoveries
 (shim_key, session_id, binding_id, actor_uri, binding_generation, intent_key, custody_json, state, confirmed_at)
 VALUES (?, ?, ?, ?, ?, ?, ?, 'recovery_pending', ?)`),
			current.ShimKey, id, binding.BindingID, binding.ActorURI, binding.BindingGeneration, binding.IntentKey, string(raw), now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, rebind(tx, `DELETE FROM session_shims WHERE session_id = ?`), id); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, rebind(tx, `UPDATE sessions SET state = 'orphaned', pid = NULL, pid_started_at = NULL, exit_code = NULL, ended_at = NULL, updated_at = ?
 WHERE id = ?`), now, id)
		return err
	})
	if errors.Is(err, ErrTeamRecoveryUnavailable) {
		return change, ErrTeamRecoveryUnavailable
	}
	return change, err
}
