package app

import (
	"context"
	"database/sql"
	"errors"

	"github.com/hollis-labs/tether/internal/session"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/teamhost"
)

// RecoverTeamSession reattaches live custody or replaces a positively lost
// execution, preserving the original actor, enrollment and accepted launch plan.
// Its caller owns the original team session receipt lock and the host lease.
// The SQL fence below independently refuses stopped/cleaned/revoked ownership.
func (s *Service) RecoverTeamSession(ctx context.Context, key, id string) error {
	unlock, err := s.lockSessionLaunch(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()
	if saved, err := s.Store.SessionReplacement(ctx, id); err == nil {
		if saved.IntentKey != key {
			return teamhost.ErrSessionUnavailable
		}
		return s.launchRetainedTeamReplacement(ctx, key, saved.ReplacementID)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if saved, err := s.Store.SessionReplacementDestination(ctx, id); err == nil {
		if saved.IntentKey != key {
			return teamhost.ErrSessionUnavailable
		}
		// The remapped receipt addresses N after commit. Resume its pending
		// placement without allocating N again or re-running provisioning.
		return s.launchRetainedTeamReplacementLocked(ctx, key, id)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
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
	var sourcePlanJSON string
	if err := s.Store.DB().QueryRowContext(ctx, `SELECT plan_json FROM launch_plans WHERE session_id=?`, id).Scan(&sourcePlanJSON); err != nil {
		return err
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
	return s.replaceRetainedTeamExecution(ctx, key, row, plan, sourcePlanJSON)
}

// Retain an existing valid team binding through the startup crash sweep. The
// runtime has been lost; original authority fences its replacement. This
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
