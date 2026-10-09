//go:build !windows

package app

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"

	"github.com/google/uuid"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/session"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/teamstore"
	"github.com/hollis-labs/tether/internal/workspace"
)

// Lost execution replacement owns no enrollment or provisioning port. Its
// original receipt lock remains held by Sessions.Recover, and the caller also
// holds the canonical source launch gate.
func (s *Service) replaceRetainedTeamExecution(ctx context.Context, key string, row *store.SessionRow, plan *launch.Plan, sourcePlanJSON string) error {
	unlock, err := s.lockSessionLaunch(ctx, "actor:"+plan.RecoveryActorURI)
	if err != nil {
		return err
	}
	defer unlock()
	if _, live := s.Manager.Get(row.ID); live {
		return store.ErrSessionReplacementUnavailable
	}
	var archived bool
	if err := s.Store.DB().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM team_gone_shim_recoveries WHERE session_id=? AND intent_key=? AND actor_uri=? AND state='recovery_pending')`, row.ID, key, plan.RecoveryActorURI).Scan(&archived); err != nil {
		return err
	}
	if !archived && (!row.PID.Valid || !recordedPIDAbsent(int(row.PID.Int64))) {
		return store.ErrSessionReplacementUnavailable
	}
	if !filepath.IsAbs(row.Workspace) || plan.RecoveryActorURI == "" {
		return store.ErrSessionReplacementUnavailable
	}
	id := uuid.NewString()
	destination := filepath.Join(filepath.Dir(row.Workspace), id)
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		return store.ErrSessionReplacementUnavailable
	}
	ws, err := workspace.Create(filepath.Dir(row.Workspace), id, plan)
	if err != nil {
		return err
	}
	ts, err := teamstore.New(s.Store.DB(), teamstore.Options{})
	if err != nil {
		return err
	}
	err = ts.WithTransaction(ctx, func(conn *sql.Conn) error {
		_, err := s.Store.PrepareTeamReplacementTx(ctx, conn, store.TeamReplacementInput{Source: *row, SourcePlanJSON: sourcePlanJSON, DestinationID: id, Workspace: ws.Root, IntentKey: key, Plan: plan, CheckedPID: row.PID.Int64, CredentialMode: s.Catalog.Global.Identity.EffectiveMode()})
		return err
	})
	if err != nil {
		return err
	}
	// Retry after any crash must read the same persisted destination. Failed
	// admission is truthful lineage, never a rollback or a second allocation.
	return s.launchRetainedTeamReplacement(ctx, key, id)
}

func (s *Service) launchRetainedTeamReplacement(ctx context.Context, key, id string) error {
	unlock, err := s.lockSessionLaunch(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()
	return s.launchRetainedTeamReplacementLocked(ctx, key, id)
}

// The canonical destination launch gate is held by the caller.
func (s *Service) launchRetainedTeamReplacementLocked(ctx context.Context, key, id string) error {
	var eligible bool
	if err := s.Store.DB().QueryRowContext(ctx, store.RetainedTeamRecoverySQL, id, id, id, id).Scan(&eligible); err != nil || !eligible {
		return store.ErrSessionReplacementUnavailable
	}
	var savedKey string
	if err := s.Store.DB().QueryRowContext(ctx, `SELECT intent_key FROM session_replacements WHERE replacement_session_id=?`, id).Scan(&savedKey); err != nil {
		return err
	}
	if savedKey != key {
		return store.ErrSessionReplacementUnavailable
	}
	if health, known := s.Manager.Health(id); known && health.Health.Alive {
		return nil
	}
	row, err := s.Store.GetSessionContext(ctx, id)
	if err != nil {
		return err
	}
	if row.State != string(session.StateCreated) {
		return store.ErrSessionReplacementUnavailable
	}
	_, err = s.launchSessionWithContext(ctx, id)
	if err != nil {
		// Before-placement admission failures are terminal for this intent;
		// retain the committed lineage instead of looping or allocating again.
		// A retained placement/unknown child remains under its existing custody.
		if _, known := s.Manager.Get(id); !known {
			if _, custodyErr := s.Store.SessionShim(ctx, id); errors.Is(custodyErr, store.ErrSessionShimNotFound) {
				_, stateErr := s.Store.DB().ExecContext(context.WithoutCancel(ctx), `UPDATE sessions SET state='failed',exit_code=1,ended_at=strftime('%Y-%m-%dT%H:%M:%fZ','now'),updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id=? AND state='created' AND COALESCE(pid,0)=0`, id)
				err = errors.Join(err, stateErr)
			}
		}
	}
	return err
}
