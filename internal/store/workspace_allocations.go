package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"time"

	"github.com/google/uuid"
)

// ErrWorkspaceAllocationConflict refuses an unproven or changed allocation.
var ErrWorkspaceAllocationConflict = errors.New("workspace allocation unavailable")

// WorkspaceScratchAllocation records an earned allocation, not launch or
// deletion authority. Reserved rows survive failed and interrupted allocation.
type WorkspaceScratchAllocation struct {
	SessionID, OperationID, SessionDigest, PlanDigest string
	Root, Status                                      string
	PhysicalIdentity                                  ScratchPhysicalIdentity
}

// ScratchPhysicalIdentity binds both the accepted parent and the allocated
// child. These observations confer no authority independently of admission.
type ScratchPhysicalIdentity struct {
	Workspace string
	Scratch   string
}

// WorkspaceLaunchDecision retains exact accepted JSON, including fields not
// understood by this version of the daemon. It is never a new launch grant.
type WorkspaceLaunchDecision struct {
	Session  SessionRow
	PlanJSON string
	// Process identity is stored outside SessionRow but remains part of the
	// complete canonical preparation predicate.
	PIDStartedAt sql.NullString
}

// ReservePreparedWorkspaceScratch binds the exact post-preparation snapshot;
// SQLite json_set ordering is retained rather than reconstructed from Plan.
func (s *Store) ReservePreparedWorkspaceScratch(ctx context.Context, decision WorkspaceLaunchDecision, operation string) (WorkspaceScratchAllocation, bool, error) {
	row := decision.Session
	want, err := scratchAllocationExpectation(decision, operation)
	if err != nil {
		return WorkspaceScratchAllocation{}, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WorkspaceScratchAllocation{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockScratchDecision(ctx, tx, want); err != nil {
		return WorkspaceScratchAllocation{}, false, err
	}
	prior, err := readScratchAllocation(ctx, tx, row.ID)
	if err == nil {
		if !sameScratchDecision(prior, want) {
			return WorkspaceScratchAllocation{}, false, ErrWorkspaceAllocationConflict
		}
		return prior, true, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return WorkspaceScratchAllocation{}, false, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO workspace_scratch_allocations
		(session_id,operation_id,session_digest,plan_digest,root,status) VALUES(?,?,?,?,?,'reserved')`,
		want.SessionID, want.OperationID, want.SessionDigest, want.PlanDigest, want.Root)
	if err != nil {
		return WorkspaceScratchAllocation{}, false, err
	}
	return want, false, tx.Commit()
}

// AdmitWorkspaceScratch records identity only while the original decision and
// the caller's physical custody still hold. validate runs under a write
// transaction and MUST NOT query this Store or reenter lifecycle operations.
// A failed check retains the reserved row; it never adopts an unknown target.
func (s *Store) AdmitWorkspaceScratch(ctx context.Context, reserved WorkspaceScratchAllocation, physicalIdentity ScratchPhysicalIdentity, validate func(context.Context) error) (WorkspaceScratchAllocation, error) {
	if reserved.Status != "reserved" || reserved.PhysicalIdentity != (ScratchPhysicalIdentity{}) || physicalIdentity.Workspace == "" || physicalIdentity.Scratch == "" || len(physicalIdentity.Workspace) > 256 || len(physicalIdentity.Scratch) > 256 || validate == nil {
		return WorkspaceScratchAllocation{}, ErrWorkspaceAllocationConflict
	}
	identityJSON, err := json.Marshal(physicalIdentity)
	if err != nil {
		return WorkspaceScratchAllocation{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WorkspaceScratchAllocation{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockScratchDecision(ctx, tx, reserved); err != nil {
		return WorkspaceScratchAllocation{}, err
	}
	prior, err := readScratchAllocation(ctx, tx, reserved.SessionID)
	if err != nil {
		return WorkspaceScratchAllocation{}, err
	}
	if !reflect.DeepEqual(prior, reserved) {
		return WorkspaceScratchAllocation{}, ErrWorkspaceAllocationConflict
	}
	if err := validate(ctx); err != nil {
		return WorkspaceScratchAllocation{}, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE workspace_scratch_allocations SET physical_identity=?,status='admitted'
		WHERE session_id=? AND operation_id=? AND status='reserved' AND physical_identity=''`,
		string(identityJSON), reserved.SessionID, reserved.OperationID)
	if err != nil {
		return WorkspaceScratchAllocation{}, err
	}
	n, err := result.RowsAffected()
	if err != nil || n != 1 {
		return WorkspaceScratchAllocation{}, ErrWorkspaceAllocationConflict
	}
	if err := tx.Commit(); err != nil {
		return WorkspaceScratchAllocation{}, err
	}
	reserved.Status, reserved.PhysicalIdentity = "admitted", physicalIdentity
	return reserved, nil
}

func scratchAllocationExpectation(decision WorkspaceLaunchDecision, operation string) (WorkspaceScratchAllocation, error) {
	row := decision.Session
	op, err := uuid.Parse(operation)
	if err != nil || op.String() != operation || row.ID == "" || row.State != "created" || (row.PID.Valid && row.PID.Int64 != 0) ||
		!json.Valid([]byte(decision.PlanJSON)) || decision.PlanJSON == "null" || !filepath.IsAbs(row.Workspace) || filepath.Clean(row.Workspace) != row.Workspace {
		return WorkspaceScratchAllocation{}, ErrWorkspaceAllocationConflict
	}
	rowDigest, err := scratchDigest(struct {
		Session      SessionRow
		PIDStartedAt sql.NullString
	}{row, decision.PIDStartedAt})
	if err != nil {
		return WorkspaceScratchAllocation{}, err
	}
	planSum := sha256.Sum256([]byte(decision.PlanJSON))
	planDigest := hex.EncodeToString(planSum[:])
	return WorkspaceScratchAllocation{SessionID: row.ID, OperationID: operation, SessionDigest: rowDigest, PlanDigest: planDigest,
		Root: filepath.Join(row.Workspace, ".tether-scratch-"+operation), Status: "reserved"}, nil
}

func scratchDigest(value any) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}

func lockScratchDecision(ctx context.Context, tx *sql.Tx, want WorkspaceScratchAllocation) error {
	// Acquire SQLite's writer reservation before reading either predicate;
	// concurrent state/plan mutations cannot commit during admission.
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET updated_at=updated_at WHERE id=?`, want.SessionID); err != nil {
		return err
	}
	current, err := readWorkspaceDecision(ctx, tx, want.SessionID)
	if err != nil {
		return err
	}
	// Compare the exact canonical JSON written at accepted creation, including
	// unknown/new fields. Rehydrating and remarshalling here would erase an
	// unrecognized field and could mistake a changed plan for the old decision.
	rawDigest := sha256.Sum256([]byte(current.PlanJSON))
	if hex.EncodeToString(rawDigest[:]) != want.PlanDigest {
		return ErrWorkspaceAllocationConflict
	}
	row := current.Session
	rowDigest, err := scratchDigest(struct {
		Session      SessionRow
		PIDStartedAt sql.NullString
	}{row, current.PIDStartedAt})
	if err != nil || rowDigest != want.SessionDigest || row.State != "created" || (row.PID.Valid && row.PID.Int64 != 0) ||
		want.Root != filepath.Join(row.Workspace, ".tether-scratch-"+want.OperationID) {
		return ErrWorkspaceAllocationConflict
	}
	var trackedCustody bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM session_shims WHERE session_id=?)`, want.SessionID).Scan(&trackedCustody); err != nil {
		return err
	}
	if trackedCustody {
		return ErrWorkspaceAllocationConflict
	}
	return nil
}

// WorkspaceLaunchDecision reads row and plan from a single SQLite snapshot.
func (s *Store) WorkspaceLaunchDecision(ctx context.Context, id string) (WorkspaceLaunchDecision, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WorkspaceLaunchDecision{}, err
	}
	defer func() { _ = tx.Rollback() }()
	decision, err := readWorkspaceDecision(ctx, tx, id)
	if err != nil {
		return WorkspaceLaunchDecision{}, err
	}
	return decision, tx.Commit()
}

// WorkspaceScratchAllocation reads retained allocation facts; it cannot
// refresh a decision, adopt a resource or issue deletion eligibility.
func (s *Store) WorkspaceScratchAllocation(ctx context.Context, id string) (WorkspaceScratchAllocation, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WorkspaceScratchAllocation{}, err
	}
	defer func() { _ = tx.Rollback() }()
	receipt, err := readScratchAllocation(ctx, tx, id)
	if err != nil {
		return WorkspaceScratchAllocation{}, err
	}
	return receipt, tx.Commit()
}

// ValidateWorkspaceScratchStart recognizes only Manager's explicit created ->
// launching transition (zero PID and its audit timestamp). All other session
// fields, exact prepared JSON and the allocation receipt must remain unchanged.
// It holds no transaction over provider entry and grants no new launch rights.
func (s *Store) ValidateWorkspaceScratchStart(ctx context.Context, accepted WorkspaceLaunchDecision, allocation WorkspaceScratchAllocation) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	current, err := readWorkspaceDecision(ctx, tx, allocation.SessionID)
	if err != nil {
		return err
	}
	expected := accepted.Session
	expected.State = "launching"
	expected.PID = sql.NullInt64{Int64: 0, Valid: true}
	expected.ExitCode = sql.NullInt64{}
	expected.UpdatedAt = current.Session.UpdatedAt
	if _, err := time.Parse(time.RFC3339, expected.UpdatedAt); err != nil || current.PlanJSON != accepted.PlanJSON || current.PIDStartedAt != accepted.PIDStartedAt || !reflect.DeepEqual(current.Session, expected) {
		return ErrWorkspaceAllocationConflict
	}
	retained, err := readScratchAllocation(ctx, tx, allocation.SessionID)
	if err != nil {
		return err
	}
	if allocation.Status != "admitted" || !reflect.DeepEqual(retained, allocation) {
		return ErrWorkspaceAllocationConflict
	}
	return tx.Commit()
}

func readWorkspaceDecision(ctx context.Context, tx *sql.Tx, id string) (WorkspaceLaunchDecision, error) {
	var decision WorkspaceLaunchDecision
	row := &decision.Session
	err := tx.QueryRowContext(ctx, `SELECT id,launch_id,project_id,logical_agent_id,provider_id,provider_kind,workspace,state,pid,exit_code,
		created_at,updated_at,ended_at,session_group_id,parent_session_id,intent,publication,workstream_id,ref_attribution,route_json,pid_started_at
		FROM sessions WHERE id=?`, id).Scan(&row.ID, &row.LaunchID, &row.ProjectID, &row.LogicalAgentID, &row.ProviderID, &row.ProviderKind,
		&row.Workspace, &row.State, &row.PID, &row.ExitCode, &row.CreatedAt, &row.UpdatedAt, &row.EndedAt, &row.SessionGroupID,
		&row.ParentSessionID, &row.Intent, &row.Publication, &row.WorkstreamID, &row.RefAttribution, &row.RouteJSON, &decision.PIDStartedAt)
	if err != nil {
		return WorkspaceLaunchDecision{}, err
	}
	err = tx.QueryRowContext(ctx, `SELECT plan_json FROM launch_plans WHERE session_id=?`, id).Scan(&decision.PlanJSON)
	return decision, err
}

func sameScratchDecision(a, b WorkspaceScratchAllocation) bool {
	return a.SessionID == b.SessionID && a.OperationID == b.OperationID && a.SessionDigest == b.SessionDigest &&
		a.PlanDigest == b.PlanDigest && a.Root == b.Root
}

func readScratchAllocation(ctx context.Context, tx *sql.Tx, id string) (WorkspaceScratchAllocation, error) {
	var allocation WorkspaceScratchAllocation
	var physicalJSON string
	err := tx.QueryRowContext(ctx, `SELECT session_id,operation_id,session_digest,plan_digest,root,physical_identity,status
		FROM workspace_scratch_allocations WHERE session_id=?`, id).Scan(&allocation.SessionID, &allocation.OperationID,
		&allocation.SessionDigest, &allocation.PlanDigest, &allocation.Root, &physicalJSON, &allocation.Status)
	if err != nil {
		return WorkspaceScratchAllocation{}, fmt.Errorf("read workspace allocation: %w", err)
	}
	if physicalJSON != "" {
		if err := json.Unmarshal([]byte(physicalJSON), &allocation.PhysicalIdentity); err != nil {
			return WorkspaceScratchAllocation{}, ErrWorkspaceAllocationConflict
		}
	}
	return allocation, nil
}
