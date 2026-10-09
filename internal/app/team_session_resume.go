package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/hollis-labs/tether/internal/session"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/teamhost"
)

// RecoverTeamSession keeps the original canonical ID and persisted launch plan.
// Its caller owns the original team session receipt lock and the host lease.
// The SQL fence below independently refuses stopped/cleaned/revoked ownership.
func (s *Service) RecoverTeamSession(ctx context.Context, key, id string) error {
	unlock, err := s.lockSessionLaunch(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()
	row, err := s.Store.GetSessionContext(ctx, id)
	if err != nil {
		return err
	}
	if health, known := s.Manager.Health(id); known && health.Health.Alive {
		return nil
	}
	if row.State == string(session.StateDetached) {
		return session.ErrDetached
	}
	if !unexpectedRecoveryEnd(row) {
		return teamhost.ErrSessionUnavailable
	}
	// A tracked shim retains process/protocol custody. Reattachment owns that
	// path; an unavailable controller is not permission for a direct child.
	if _, err := s.Store.SessionShim(ctx, id); err == nil {
		return teamhost.ErrSessionUnavailable
	} else if !errors.Is(err, store.ErrSessionShimNotFound) {
		return err
	}
	plan, err := s.Store.GetLaunchPlan(id)
	if err != nil {
		return err
	}
	if !plan.TeamMember {
		return teamhost.ErrInvalidRequest
	}
	var actor string
	err = s.Store.DB().QueryRowContext(ctx, `SELECT json_extract(request,'$.Actor') FROM team_port_intents WHERE port_kind='session' AND intent_key=? AND ended='' AND state='done' AND CAST(payload AS TEXT)=json_quote(?)`, key, id).Scan(&actor)
	if err != nil {
		return teamhost.ErrSessionUnavailable
	}
	plan.ResumeSourceSessionID = id
	plan.RecoveryActorURI = actor
	mapping, err := s.Store.GetSessionProviderMapping(id, "tether", plan.ProviderID)
	if err != nil && !errors.Is(err, store.ErrProviderMappingNotFound) {
		return err
	}
	plan.ResumeProviderSessionID = mapping.NativeSessionID.String
	ck, err := s.Store.GetLatestCheckpointForAgent(plan.LogicalAgentID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	pack := s.recoveryContext(ctx, plan, ck)
	plan.RecoveryPrompt = pack.prompt(buildResumePrompt(ck, plan.BootPrompt))
	raw, err := json.Marshal(plan)
	if err != nil {
		return err
	}
	// Recheck all durable ownership inside the same write transaction as the
	// same-ID state transition. No binding generation or enrollment is created.
	tx, err := s.Store.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var eligible bool
	if err := tx.QueryRowContext(ctx, store.RetainedTeamRecoverySQL, id, id, id, id).Scan(&eligible); err != nil {
		return err
	}
	if !eligible {
		return teamhost.ErrSessionUnavailable
	}
	result, err := tx.ExecContext(ctx, `UPDATE sessions SET state='created',pid=0,exit_code=NULL,ended_at=NULL WHERE id=? AND (state='orphaned' OR (state='failed' AND exit_code=-1))
 AND EXISTS(SELECT 1 FROM team_host_intents WHERE intent_key=? AND tombstone='' AND cleaned=0 AND dead=0 AND json_extract(member,'$.session_id')=? AND json_extract(member,'$.actor')=?)
 AND EXISTS(SELECT 1 FROM team_port_intents WHERE port_kind='session' AND intent_key=? AND ended='' AND state='done' AND CAST(payload AS TEXT)=json_quote(?))
 AND EXISTS(SELECT 1 FROM team_port_intents e JOIN runtime_bindings b ON b.attempt_id=e.binding_secret AND b.target_urn=e.acquired_urn WHERE e.port_kind='enrollment' AND e.intent_key=? AND e.ended='' AND e.binding_ended=0 AND e.state='done' AND b.session_id=? AND b.target_urn=? AND b.host_id='team' AND b.visibility='tether-hosted' AND b.revoked_at IS NULL AND (b.lease_expires_at IS NULL OR b.lease_expires_at>strftime('%Y-%m-%dT%H:%M:%fZ','now')) AND b.generation=(SELECT MAX(generation) FROM runtime_bindings WHERE target_urn=b.target_urn))`, id, key, id, actor, key, id, key, id, actor)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return teamhost.ErrSessionUnavailable
	}
	if _, err = tx.ExecContext(ctx, `UPDATE launch_plans SET plan_json=? WHERE session_id=?`, string(raw), id); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	_, err = s.launchSessionWithContext(ctx, id)
	return err
}

// Retain an existing valid team binding through the startup crash sweep. The
// runtime has been lost, but the original session is its recovery target. This
// is not permission to recover a stopped or explicitly revoked member.
func (s *Service) retainedTeamRecoveryEligible(ctx context.Context, row *store.SessionRow) bool {
	if !unexpectedRecoveryEnd(row) {
		return false
	}
	plan, err := s.Store.GetLaunchPlan(row.ID)
	if err != nil || !plan.TeamMember {
		return false
	}
	var eligible bool
	err = s.Store.DB().QueryRowContext(ctx, store.RetainedTeamRecoverySQL, row.ID, row.ID, row.ID, row.ID).Scan(&eligible)
	return err == nil && eligible
}

func unexpectedRecoveryEnd(row *store.SessionRow) bool {
	return row.State == string(session.StateOrphaned) || row.State == string(session.StateFailed) && row.ExitCode.Valid && row.ExitCode.Int64 == -1
}
