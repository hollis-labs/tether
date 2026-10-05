package shimretire

import (
	"context"
	"errors"
	"os"
	"slices"
	"time"
)

// ConfinedCleanup freezes preflight evidence and vetoes; its callbacks cannot
// authorize an unlink. The owned concrete Admission must establish actual
// current native custody, SQL eligibility and original proof freshness together.
// Root references and interface assertions are not custody capabilities.
type ConfinedCleanup struct {
	Root                             *os.Root
	RootID, OwnerID, CustodyRevision string
	RootIdentity                     FileIdentity
	Store                            Store
	Validate                         func(context.Context, Request) error
	Observe                          func(context.Context) (Snapshot, Observation, error)
	Now                              func() time.Time
	Admission                        CleanupAdmission
}

func (c ConfinedCleanup) Descriptor(ctx context.Context, operation string, a Artifact) (Mutation, error) {
	return c.remove(ctx, operation, a, nil)
}
func (c ConfinedCleanup) remove(ctx context.Context, operation string, a Artifact, cursor *SweepCursor) (Mutation, error) {
	if !ownedAdmission(c.Admission) {
		return NoChange, ErrCleanupUnsupported
	}
	retention := cursor != nil
	refuse := func() (Mutation, error) { return NoChange, errors.New("descriptor cleanup custody unproved") }
	if c.Root == nil || c.Store == nil || c.Validate == nil || c.Observe == nil || c.Now == nil || !relative(a.RelativePath) || (!retention && a.Category != Descriptor) || (retention && (!payloadCategory(a.Category) || a.Category == Descriptor)) || a.RootID != c.RootID || a.OwnerID != c.OwnerID || a.CustodyRevision != c.CustodyRevision {
		return refuse()
	}
	receipt, err := c.Store.Load(ctx, operation)
	if err != nil || ValidateReceipt(receipt) != nil || !receipt.Snapshot.Retired || (!retention && (receipt.Snapshot.Descriptor != a || phaseBeforeCleanup(receipt.Phase))) || (retention && (receipt.Phase != StateReconciled || len(receipt.Obligations) != 0 || !slices.Contains(receipt.Inventory, a))) {
		return refuse()
	}
	if ctx.Err() != nil || c.Validate(ctx, receipt.Request) != nil || ctx.Err() != nil {
		return refuse()
	}
	snapshot, proof, err := c.Observe(ctx)
	if err != nil || ctx.Err() != nil || snapshot != receipt.Snapshot || Verify(receipt.Request, snapshot, proof, c.Now()).Outcome != Eligible {
		return refuse()
	}
	if ctx.Err() != nil || c.Validate(ctx, receipt.Request) != nil || ctx.Err() != nil {
		return refuse()
	}
	// Callbacks may durably change recovery obligations. Bind the final read to
	// the audit that authorized this operation before any filesystem effect.
	current, err := c.Store.Load(ctx, operation)
	if err != nil || ctx.Err() != nil || ValidateReceipt(current) != nil || current.Version != receipt.Version || current.OperationID != receipt.OperationID || current.RequestDigest != receipt.RequestDigest || current.Revision != receipt.Revision || current.Snapshot != receipt.Snapshot || current.Phase != receipt.Phase || !current.RetiredAt.Equal(receipt.RetiredAt) || !slices.Equal(current.Inventory, receipt.Inventory) || !slices.Equal(current.Obligations, receipt.Obligations) || (retention && len(current.Obligations) != 0) {
		return refuse()
	}
	// Final authority and durable reads may exhaust the original proof budget.
	// Sample the trusted clock before the final physical custody observations.
	if Verify(receipt.Request, snapshot, proof, c.Now()).Outcome != Eligible || ctx.Err() != nil {
		return refuse()
	}
	mode := DescriptorCleanup
	var frozenCursor *SweepCursor
	if cursor != nil {
		mode = RetentionCleanup
		v := cursor.Clone()
		frozenCursor = &v
	}
	proof.Contradictions = slices.Clone(proof.Contradictions)
	return c.Admission.RemoveOwned(ctx, CleanupAttempt{Mode: mode, Receipt: receipt.Clone(), Snapshot: snapshot, Proof: proof, Artifact: a, Cursor: frozenCursor, Root: c.Root, RootIdentity: c.RootIdentity})
}

func phaseBeforeCleanup(p Phase) bool {
	return p != RetirementCommitted && p != DescriptorCleanupComplete && p != StateReconciled
}

// RetentionRemoval is the confined filesystem effect behind a trusted complete
// retention lease. It requires the exact durable item intent, current policy
// authority, current hold/inventory/proof observation and registered audit
// inventory. No operational adapter is installed by constructing this seam.
type RetentionRemoval struct {
	Cleanup  ConfinedCleanup
	Request  SweepRequest
	Cursors  CursorStore
	Validate func(context.Context, SweepRequest) error
	Observe  func(context.Context, SweepCandidate) (SweepCandidate, Snapshot, Observation, error)
}

func (r RetentionRemoval) Remove(ctx context.Context, candidate SweepCandidate) (Mutation, error) {
	if !ownedAdmission(r.Cleanup.Admission) {
		return NoChange, ErrCleanupUnsupported
	}
	refuse := func() (Mutation, error) { return NoChange, errors.New("retention cleanup custody unproved") }
	if ValidateSweepRequest(r.Request) != nil || r.Cursors == nil || r.Validate == nil || r.Observe == nil || r.Cleanup.Validate == nil || r.Cleanup.Store == nil || r.Cleanup.Now == nil {
		return refuse()
	}
	cursor, err := r.Cursors.LoadCursor(ctx, r.Request.OperationID)
	if err != nil || ValidateSweepCursor(cursor) != nil || cursor.RequestDigest != retentionDigest(r.Request) || cursor.Phase != CursorIntent || cursor.Pending == nil || *cursor.Pending != candidate || candidate.Hold != NoRetentionHold || !text(candidate.HoldRevision) || !slices.Contains(r.Request.Policy.Categories, candidate.Artifact.Category) || !slices.Contains(r.Request.Policy.RootIDs, candidate.Artifact.RootID) || ((candidate.Artifact.Category == Journal || candidate.Artifact.Category == Bridge) && !text(r.Request.Policy.ReplayWaiver)) {
		return refuse()
	}
	audit, err := r.Cleanup.Store.Load(ctx, candidate.RetirementOperation)
	if err != nil || ValidateReceipt(audit) != nil || audit.Phase != StateReconciled || audit.RetiredAt.After(r.Cleanup.Now()) || (r.Request.Policy.MinRetiredAge != nil && r.Cleanup.Now().Sub(audit.RetiredAt) < *r.Request.Policy.MinRetiredAge) {
		return refuse()
	}
	cleanup := r.Cleanup
	originalValidate := cleanup.Validate
	cleanup.Validate = func(ctx context.Context, request Request) error {
		if ctx.Err() != nil || r.Validate(ctx, r.Request.Clone()) != nil || ctx.Err() != nil {
			return errors.New("retention authority unproved")
		}
		if originalValidate(ctx, request) != nil || ctx.Err() != nil {
			return errors.New("retention authority unproved")
		}
		current, err := r.Cursors.LoadCursor(ctx, r.Request.OperationID)
		if err != nil || ValidateSweepCursor(current) != nil || current.RequestDigest != cursor.RequestDigest || current.InventoryRevision != cursor.InventoryRevision || current.Phase != CursorIntent || current.Pending == nil || *current.Pending != candidate {
			return errors.New("retention intent changed")
		}
		return nil
	}
	cleanup.Observe = func(ctx context.Context) (Snapshot, Observation, error) {
		current, snapshot, proof, err := r.Observe(ctx, candidate)
		if err != nil || ctx.Err() != nil || current != candidate || current.Hold != NoRetentionHold {
			return Snapshot{}, Observation{}, errors.New("retention inventory changed")
		}
		return snapshot, proof, nil
	}
	return cleanup.remove(ctx, candidate.RetirementOperation, candidate.Artifact, &cursor)
}
