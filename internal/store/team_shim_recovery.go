package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

var ErrTeamShimRecoveryUnavailable = errors.New("team shim recovery unavailable")

// TeamShimRecoveryFence is a retained binding/custody snapshot, never a grant.
// Only the host's confirmed Gone+retired path may consume it. The transaction
// independently rechecks all durable ownership and exact custody metadata.
type TeamShimRecoveryFence struct {
	Custody                        SessionShimRow
	BindingID, ActorURI, IntentKey string
	BindingGeneration              int64
}

type teamRecoveryQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func teamShimRecoveryFence(ctx context.Context, q teamRecoveryQuerier, custody SessionShimRow) (TeamShimRecoveryFence, error) {
	f := TeamShimRecoveryFence{Custody: custody}
	var eligible bool
	if err := q.QueryRowContext(ctx, RetainedTeamRecoverySQL, custody.SessionID, custody.SessionID, custody.SessionID, custody.SessionID).Scan(&eligible); err != nil {
		return f, err
	}
	if !eligible {
		return f, ErrTeamShimRecoveryUnavailable
	}
	var matches int
	err := q.QueryRowContext(ctx, `SELECT b.id,b.target_urn,b.generation,e.intent_key,COUNT(*) OVER() `+retainedTeamRecoveryFrom, custody.SessionID, custody.SessionID, custody.SessionID, custody.SessionID).Scan(&f.BindingID, &f.ActorURI, &f.BindingGeneration, &f.IntentKey, &matches)
	if matches != 1 {
		return f, ErrTeamShimRecoveryUnavailable
	}
	if err != nil {
		return f, ErrTeamShimRecoveryUnavailable
	}
	return f, nil
}

func (s *Store) GoneTeamShimRecoveryFence(ctx context.Context, custody SessionShimRow) (TeamShimRecoveryFence, error) {
	current, err := s.SessionShim(ctx, custody.SessionID)
	if err != nil || current != custody {
		return TeamShimRecoveryFence{}, ErrTeamShimRecoveryUnavailable
	}
	var eligible bool
	err = s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sessions s JOIN launch_plans l ON l.session_id=s.id WHERE s.id=? AND s.state IN ('launching','running','detached') AND json_extract(l.plan_json,'$.team_member')=1)`, custody.SessionID).Scan(&eligible)
	if err != nil || !eligible {
		return TeamShimRecoveryFence{}, ErrTeamShimRecoveryUnavailable
	}
	return teamShimRecoveryFence(ctx, s.db, custody)
}

// CommitGoneTeamShimRecovery is called after matching canonical placement
// retirement and positive host/provider absence. It archives only secret-free
// custody, then releases that exact old active row and marks recovery pending.
// It never revokes/un-revokes a principal, binding or enrollment.
func (s *Store) CommitGoneTeamShimRecovery(ctx context.Context, expected TeamShimRecoveryFence) (SessionStateChange, error) {
	id := expected.Custody.SessionID
	change := SessionStateChange{SessionID: id, To: "orphaned", Reason: "team_shim_recovery_pending"}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return change, err
	}
	defer func() { _ = tx.Rollback() }()
	current, err := scanShim(tx.QueryRowContext(ctx, `SELECT `+shimColumns+` FROM session_shims WHERE session_id=?`, id))
	if err != nil || current != expected.Custody {
		return change, ErrTeamShimRecoveryUnavailable
	}
	f, err := teamShimRecoveryFence(ctx, tx, current)
	if err != nil {
		return change, err
	}
	if f != expected {
		return change, ErrTeamShimRecoveryUnavailable
	}
	var team bool
	err = tx.QueryRowContext(ctx, `SELECT s.state,COALESCE(s.logical_agent_id,''),COALESCE(json_extract(l.plan_json,'$.team_member'),0) FROM sessions s JOIN launch_plans l ON l.session_id=s.id WHERE s.id=?`, id).Scan(&change.From, &change.LogicalAgentID, &team)
	if err != nil || !team {
		return change, ErrTeamShimRecoveryUnavailable
	}
	switch change.From {
	case "launching", "running", "detached":
	default:
		return change, ErrTeamShimRecoveryUnavailable
	}
	raw, err := json.Marshal(current)
	if err != nil {
		return change, err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err = tx.ExecContext(ctx, `INSERT INTO team_gone_shim_recoveries(shim_key,session_id,binding_id,actor_uri,binding_generation,intent_key,custody_json,state,confirmed_at) VALUES(?,?,?,?,?,?,?,'recovery_pending',?)`, current.ShimKey, id, f.BindingID, f.ActorURI, f.BindingGeneration, f.IntentKey, string(raw), now); err != nil {
		return change, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM session_shims WHERE session_id=?`, id); err != nil {
		return change, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE sessions SET state='orphaned',pid=NULL,pid_started_at=NULL,exit_code=NULL,ended_at=NULL,updated_at=? WHERE id=?`, now, id); err != nil {
		return change, err
	}
	return change, tx.Commit()
}
