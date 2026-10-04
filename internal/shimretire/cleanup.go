package shimretire

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// ConfinedCleanup operates under the caller's complete exclusive custody lease.
// Root is an already-open canonical private root; its identity and ownership
// were obtained by the trusted inventory adapter. No arbitrary paths or caller
// absence assertion authorize deletion. Validate must recheck the held lease
// and current authority. It runs before the final filesystem observation.
type ConfinedCleanup struct {
	Root                             *os.Root
	RootID, OwnerID, CustodyRevision string
	RootIdentity                     FileIdentity
	Store                            Store
	Validate                         func(context.Context, Request) error
	Observe                          func(context.Context) (Snapshot, Observation, error)
	Now                              func() time.Time
}

func (c ConfinedCleanup) Descriptor(ctx context.Context, operation string, a Artifact) (Mutation, error) {
	return c.remove(ctx, operation, a, false)
}
func (c ConfinedCleanup) remove(ctx context.Context, operation string, a Artifact, retention bool) (Mutation, error) {
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
	// The trusted Observe seam includes current complete proof and authority under
	// exclusive custody. It cannot manufacture success for Unsupported backends.
	// No callback follows the filesystem observations below. The custody contract must
	// exclude concurrent writers through unlink and directory durability.
	root, err := c.Root.Lstat(".")
	if err != nil || fileIdentity(root) != c.RootIdentity || !root.IsDir() || root.Mode().Perm() != 0700 {
		return refuse()
	}
	namedRoot, e := os.Lstat(c.Root.Name())
	canonical, e2 := filepath.EvalSymlinks(c.Root.Name())
	if e != nil || e2 != nil || !filepath.IsAbs(c.Root.Name()) || canonical != c.Root.Name() || !os.SameFile(root, namedRoot) || !namedRoot.IsDir() {
		return refuse()
	}
	if !confinedParents(c.Root, a.RelativePath) {
		return refuse()
	}
	info, err := c.Root.Lstat(a.RelativePath)
	if os.IsNotExist(err) {
		return NoChange, nil
	}
	if a.Size > math.MaxInt64 {
		return refuse()
	}
	expectedSize := int64(a.Size)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || fileIdentity(info) != a.Identity || info.Size() < 0 || info.Size() != expectedSize {
		return refuse()
	}
	if ctx.Err() != nil {
		return refuse()
	}
	if err = c.Root.Remove(a.RelativePath); err != nil {
		return Uncertain, errors.New("descriptor cleanup outcome uncertain")
	}
	// Sync the containing directory; failure retains a pending cleanup obligation.
	parent, err := c.Root.Open(parentPath(a.RelativePath))
	if err != nil {
		return Uncertain, errors.New("descriptor cleanup durability uncertain")
	}
	defer func() { _ = parent.Close() }()
	if parent.Sync() != nil || ctx.Err() != nil {
		return Uncertain, errors.New("descriptor cleanup durability uncertain")
	}
	return Changed, nil
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
	return cleanup.remove(ctx, candidate.RetirementOperation, candidate.Artifact, true)
}
