package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"time"

	"github.com/google/uuid"
)

// WorkspacePreparationStamp contains only the values produced by this admitted
// Prepare. It cannot change provider, roots, caller environment or authority.
type WorkspacePreparationStamp struct {
	RefAttribution, NativeStateRoot string
}

// WorkspacePreparationResult preserves the existing best-effort attribution
// behavior while native-state stamping remains mandatory.
type WorkspacePreparationResult struct {
	Decision            WorkspaceLaunchDecision
	RefAttributionError error
}

// RecordWorkspacePreparation compares the original exact row/JSON before
// recording the two existing computed preparation stamps. nil validates an
// unchanged ACP preparation. Any intervening change refuses, without refresh.
func (s *Store) RecordWorkspacePreparation(ctx context.Context, before WorkspaceLaunchDecision, stamp *WorkspacePreparationStamp) (WorkspacePreparationResult, error) {
	want, err := scratchAllocationExpectation(before, uuid.NewString())
	if err != nil {
		return WorkspacePreparationResult{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WorkspacePreparationResult{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockScratchDecision(ctx, tx, want); err != nil {
		return WorkspacePreparationResult{}, err
	}
	prior, receiptErr := readScratchAllocation(ctx, tx, before.Session.ID)
	if receiptErr == nil {
		expected, err := scratchAllocationExpectation(before, prior.OperationID)
		if err != nil || prior.Status != "admitted" || !sameScratchDecision(prior, expected) {
			return WorkspacePreparationResult{}, ErrWorkspaceAllocationConflict
		}
		if stamp != nil {
			var nativeRoot string
			if err := tx.QueryRowContext(ctx, `SELECT COALESCE(json_extract(?, '$.native_state_root'),'')`, before.PlanJSON).Scan(&nativeRoot); err != nil {
				return WorkspacePreparationResult{}, err
			}
			if !before.Session.RefAttribution.Valid || before.Session.RefAttribution.String != stamp.RefAttribution || nativeRoot != stamp.NativeStateRoot {
				return WorkspacePreparationResult{}, ErrWorkspaceAllocationConflict
			}
		}
		// An earned retry retains its ORIGINAL snapshot/timestamp. It cannot
		// refresh an existing receipt with newly computed preparation fields.
		return WorkspacePreparationResult{Decision: before}, tx.Commit()
	}
	if !errors.Is(receiptErr, sql.ErrNoRows) {
		return WorkspacePreparationResult{}, receiptErr
	}
	var refErr error
	if stamp != nil {
		if stamp.RefAttribution == "" || !filepath.IsAbs(stamp.NativeStateRoot) || filepath.Clean(stamp.NativeStateRoot) != stamp.NativeStateRoot {
			return WorkspacePreparationResult{}, ErrWorkspaceAllocationConflict
		}
		after := before
		updatedAt := time.Now().UTC().Format(time.RFC3339)
		if err := tx.QueryRowContext(ctx, `SELECT json_set(?, '$.native_state_root', ?)`, before.PlanJSON, stamp.NativeStateRoot).Scan(&after.PlanJSON); err != nil {
			return WorkspacePreparationResult{}, err
		}
		if _, err := tx.ExecContext(ctx, `SAVEPOINT scratch_ref_stamp`); err != nil {
			return WorkspacePreparationResult{}, err
		}
		result, attributionErr := tx.ExecContext(ctx, `UPDATE sessions SET ref_attribution=?,updated_at=? WHERE id=?`, stamp.RefAttribution, updatedAt, before.Session.ID)
		refErr = attributionErr
		if refErr == nil {
			n, affectedErr := result.RowsAffected()
			refErr = affectedErr
			if refErr == nil && n != 1 {
				refErr = ErrSessionNotFound
			}
		}
		if refErr != nil {
			if _, err := tx.ExecContext(ctx, `ROLLBACK TO scratch_ref_stamp`); err != nil {
				return WorkspacePreparationResult{}, err
			}
		} else {
			after.Session.RefAttribution = sql.NullString{String: stamp.RefAttribution, Valid: true}
			after.Session.UpdatedAt = updatedAt
		}
		if _, err := tx.ExecContext(ctx, `RELEASE scratch_ref_stamp`); err != nil {
			return WorkspacePreparationResult{}, err
		}
		result, err = tx.ExecContext(ctx, `UPDATE launch_plans SET plan_json=? WHERE session_id=?`, after.PlanJSON, before.Session.ID)
		if err != nil {
			return WorkspacePreparationResult{}, err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return WorkspacePreparationResult{}, err
		}
		if n != 1 {
			return WorkspacePreparationResult{}, ErrSessionNotFound
		}
		before = after
	}
	actual, err := readWorkspaceDecision(ctx, tx, before.Session.ID)
	if err != nil {
		return WorkspacePreparationResult{}, err
	}
	if !reflect.DeepEqual(actual, before) {
		return WorkspacePreparationResult{}, ErrWorkspaceAllocationConflict
	}
	return WorkspacePreparationResult{Decision: before, RefAttributionError: refErr}, tx.Commit()
}

// BeginWorkspaceScratchPlacement performs the existing created -> launching
// transition against the exact admitted snapshot. It returns only the bytes and
// audit timestamp produced by this transaction, never a refreshed decision.
func (s *Store) BeginWorkspaceScratchPlacement(ctx context.Context, accepted WorkspaceLaunchDecision, allocation WorkspaceScratchAllocation) (WorkspaceLaunchDecision, error) {
	want, err := scratchAllocationExpectation(accepted, allocation.OperationID)
	if err != nil || !sameScratchDecision(want, allocation) {
		return WorkspaceLaunchDecision{}, ErrWorkspaceAllocationConflict
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WorkspaceLaunchDecision{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET updated_at=updated_at WHERE id=?`, accepted.Session.ID); err != nil {
		return WorkspaceLaunchDecision{}, err
	}
	if err := validateExactScratchDecision(ctx, tx, accepted, allocation); err != nil {
		return WorkspaceLaunchDecision{}, err
	}
	after := accepted
	after.Session.State = "launching"
	after.Session.PID = sql.NullInt64{Int64: 0, Valid: true}
	after.Session.ExitCode = sql.NullInt64{}
	after.Session.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET state='launching',pid=0,exit_code=NULL,updated_at=? WHERE id=?`, after.Session.UpdatedAt, accepted.Session.ID); err != nil {
		return WorkspaceLaunchDecision{}, err
	}
	if err := validateExactScratchDecision(ctx, tx, after, allocation); err != nil {
		return WorkspaceLaunchDecision{}, err
	}
	return after, tx.Commit()
}

// ValidateWorkspaceScratchPlacement checks the exact transition returned above
// immediately before placement. No transaction spans a provider callback.
func (s *Store) ValidateWorkspaceScratchPlacement(ctx context.Context, accepted WorkspaceLaunchDecision, allocation WorkspaceScratchAllocation) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := validateExactScratchDecision(ctx, tx, accepted, allocation); err != nil {
		return err
	}
	return tx.Commit()
}

func validateExactScratchDecision(ctx context.Context, tx *sql.Tx, accepted WorkspaceLaunchDecision, allocation WorkspaceScratchAllocation) error {
	if accepted.Session.ID != allocation.SessionID || allocation.Status != "admitted" {
		return ErrWorkspaceAllocationConflict
	}
	current, err := readWorkspaceDecision(ctx, tx, allocation.SessionID)
	if err != nil {
		return err
	}
	retained, err := readScratchAllocation(ctx, tx, allocation.SessionID)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(current, accepted) || !reflect.DeepEqual(retained, allocation) {
		return ErrWorkspaceAllocationConflict
	}
	return nil
}
