package shimretire

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
)

var ErrNotFound = errors.New("retirement not found")

type Phase string

const (
	IntentRecorded            Phase = "intent_recorded"
	RetirementCommitted       Phase = "retirement_committed"
	DescriptorCleanupComplete Phase = "descriptor_cleanup_complete"
	StateReconciled           Phase = "state_reconciled"
)

type Mutation string

const (
	NoChange  Mutation = "no_change"
	Changed   Mutation = "changed"
	Uncertain Mutation = "uncertain"
)

// Receipt is a nonsecret tombstone retained outside all deletable payloads.
// A successful Store.Record must durably persist the exact supplied value.
type Receipt struct {
	Version, OperationID, RequestDigest, Revision string
	Request                                       Request
	Snapshot                                      Snapshot
	Proof                                         Observation
	Phase                                         Phase
	RetiredAt                                     time.Time
	Obligations                                   []string
	Inventory                                     []Artifact
}

func (r Receipt) Clone() Receipt {
	r.Proof.Contradictions = append([]string(nil), r.Proof.Contradictions...)
	r.Obligations = append([]string(nil), r.Obligations...)
	r.Inventory = append([]Artifact(nil), r.Inventory...)
	return r
}

type Store interface {
	Load(context.Context, string) (Receipt, error)
	Record(context.Context, Receipt, string) (string, error)
}

// Lease excludes submission, controller takeover and lifecycle changes until
// Close. Methods must refresh named-file custody and honor their contexts.
// Mutators must retain the submission fence and recheck actual complete absence,
// authority and custody after the final callback before any effect.
type Lease interface {
	Observe(context.Context) (Snapshot, Observation, error)
	CommitRetired(context.Context, Snapshot) (Mutation, error)
	CleanupDescriptor(context.Context, Artifact) (Mutation, error)
	ReconcileState(context.Context, Placement, string) error
	Close() error
}
type Ports struct {
	Store    Store
	Acquire  func(context.Context, Placement) (Lease, error)
	Validate func(context.Context, Request) error
	Now      func() time.Time
}

func RetireAbsent(ctx context.Context, r Request, p Ports) (out Result) {
	fail := func(o Outcome, code, obligation string) Result {
		return Result{Outcome: o, Code: code, Obligations: []string{obligation}}
	}
	if !validRequest(r) || p.Store == nil || p.Acquire == nil || p.Validate == nil || p.Now == nil {
		return fail(RefusedIdentity, "binding_refused", "placement_unknown")
	}
	raw, _ := json.Marshal(r)
	hash := sha256.Sum256(raw)
	digest := hex.EncodeToString(hash[:])
	saved, err := p.Store.Load(ctx, r.OperationID)
	exists := err == nil
	if err != nil && !errors.Is(err, ErrNotFound) {
		return fail(RetainedUnknown, "receipt_unavailable", "receipt_pending")
	}
	if exists {
		if !receiptBound(r, digest, saved) {
			return fail(RefusedIdentity, "receipt_binding", "placement_unknown")
		}
		if saved.Phase == StateReconciled {
			return Result{Outcome: AlreadyRetired, Code: "retirement_complete", Obligations: append([]string(nil), saved.Obligations...)}
		}
	}
	if ctx.Err() != nil || p.Validate(ctx, r) != nil || ctx.Err() != nil {
		return fail(RetainedUnknown, "authority_refused", "placement_unknown")
	}
	lease, err := p.Acquire(ctx, r.Placement)
	if err != nil || lease == nil {
		return fail(RetainedUnknown, "lease_unavailable", "placement_unknown")
	}
	defer func() {
		for _, obligation := range saved.Obligations {
			if !contains(out.Obligations, obligation) {
				out.Obligations = append(out.Obligations, obligation)
			}
		}
		if lease.Close() != nil {
			out.Obligations = append(out.Obligations, "lease_close_pending")
			if out.Outcome == Retired {
				out.Outcome = RetiredCleanupPending
				out.Code = "lease_close_pending"
			}
		}
		// Persist pending facts through a bounded cancellation-detached context.
		// Receipt failure does not permit cleanup or discard retained obligations.
		if len(out.Obligations) > 0 && saved.Revision != "" {
			pending := saved.Clone()
			for _, obligation := range out.Obligations {
				if !contains(pending.Obligations, obligation) {
					pending.Obligations = append(pending.Obligations, obligation)
				}
			}
			final, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			if _, e := p.Store.Record(final, pending, saved.Revision); e != nil || final.Err() != nil {
				if !contains(out.Obligations, "receipt_pending") {
					out.Obligations = append(out.Obligations, "receipt_pending")
				}
			}
		}
	}()
	fresh := func() (Snapshot, Observation, Result) {
		if ctx.Err() != nil || p.Validate(ctx, r) != nil || ctx.Err() != nil {
			return Snapshot{}, Observation{}, fail(RetainedUnknown, "authority_refused", "placement_unknown")
		}
		s, o, e := lease.Observe(ctx)
		if e != nil || ctx.Err() != nil {
			return s, o, fail(RetainedUnknown, "observation_unavailable", "placement_unknown")
		}
		// Recheck authority after the observation callback. Concrete mutations must
		// revalidate actual custody and proof under the complete exclusion lease.
		verified := Verify(r, s, o, p.Now())
		if ctx.Err() != nil || p.Validate(ctx, r) != nil || ctx.Err() != nil {
			return s, o, fail(RetainedUnknown, "authority_refused", "placement_unknown")
		}
		return s, o, verified
	}
	s, o, verified := fresh()
	if verified.Outcome != Eligible {
		return verified
	}
	if !exists && s.Retired {
		return fail(RefusedIdentity, "unbound_retirement", "placement_unknown")
	}
	if exists {
		expected := saved.Snapshot
		expected.Retired = s.Retired
		if expected != s {
			return fail(RefusedIdentity, "inventory_changed", "placement_unknown")
		}
	} else {
		saved = Receipt{Version: Version, OperationID: r.OperationID, RequestDigest: digest, Request: r, Snapshot: s, Proof: o, Phase: IntentRecorded, Inventory: []Artifact{s.Descriptor}}
	}
	record := func() bool {
		revision, e := p.Store.Record(ctx, saved.Clone(), saved.Revision)
		if e != nil || revision == "" {
			return false
		}
		saved.Revision = revision
		return ctx.Err() == nil
	}
	if !exists && !record() {
		return fail(RetainedUnknown, "intent_failed", "receipt_pending")
	}
	if saved.Phase == IntentRecorded {
		s, _, verified = fresh()
		if verified.Outcome != Eligible {
			return verified
		}
		expected := saved.Snapshot
		expected.Retired = s.Retired
		if expected != s {
			return fail(RefusedIdentity, "inventory_changed", "placement_unknown")
		}
		if !s.Retired {
			mutation, e := lease.CommitRetired(ctx, s)
			if e != nil || (mutation != Changed && mutation != NoChange) || ctx.Err() != nil {
				return fail(RetainedUnknown, "retirement_commit_unknown", "retirement_commit_unknown")
			}
		}
		committed, proof, check := fresh()
		if check.Outcome != Eligible || !committed.Retired {
			return fail(RetainedUnknown, "retirement_commit_unproved", "retirement_commit_unknown")
		}
		expected = s
		expected.Retired = true
		if committed != expected {
			return fail(RefusedIdentity, "retirement_identity_changed", "retirement_commit_unknown")
		}
		saved.Snapshot = committed
		saved.Proof = proof
		saved.RetiredAt = p.Now()
		saved.Phase = RetirementCommitted
		if !record() {
			return fail(RetiredCleanupPending, "retirement_receipt_pending", "receipt_pending")
		}
	}
	if saved.Phase == RetirementCommitted {
		s, _, verified = fresh()
		if verified.Outcome != Eligible || s != saved.Snapshot || !s.Retired {
			return fail(RetiredCleanupPending, "cleanup_custody_unproved", "cleanup_pending")
		}
		mutation, e := lease.CleanupDescriptor(ctx, saved.Snapshot.Descriptor)
		if e != nil || (mutation != Changed && mutation != NoChange) || ctx.Err() != nil {
			return fail(RetiredCleanupPending, "cleanup_uncertain", "cleanup_pending")
		}
		saved.Phase = DescriptorCleanupComplete
		if !record() {
			return fail(RetiredCleanupPending, "cleanup_receipt_pending", "receipt_pending")
		}
	}
	if saved.Phase == DescriptorCleanupComplete {
		s, _, verified = fresh()
		if verified.Outcome != Eligible || s != saved.Snapshot || !s.Retired {
			return fail(RetiredStatePending, "state_custody_unproved", "state_reconcile_pending")
		}
		if lease.ReconcileState(ctx, r.Placement, r.OperationID) != nil || ctx.Err() != nil {
			return fail(RetiredStatePending, "state_reconcile_pending", "state_reconcile_pending")
		}
		saved.Phase = StateReconciled
		if !record() {
			return fail(RetiredStatePending, "state_receipt_pending", "receipt_pending")
		}
	}
	return Result{Outcome: Retired, Code: "retirement_complete", Obligations: append([]string(nil), saved.Obligations...)}
}

func receiptBound(r Request, digest string, s Receipt) bool {
	if s.Version != Version || s.OperationID != r.OperationID || s.RequestDigest != digest || s.Request != r || !text(s.Revision) || s.Snapshot.Placement != r.Placement || len(s.Obligations) > 256 || len(s.Inventory) > 256 {
		return false
	}
	for i, a := range s.Inventory {
		if !relative(a.RelativePath) || !text(a.RootID, a.OwnerID, a.CustodyRevision, a.InventoryRevision) || a.InventoryRevision != s.Snapshot.InventoryRevision || a.Identity.Kind != "regular" || a.Identity.Inode == 0 || !payloadCategory(a.Category) {
			return false
		}
		if a.Category == Descriptor && a != s.Snapshot.Descriptor {
			return false
		}
		if a.RootID == s.Snapshot.Descriptor.RootID && a.RelativePath == s.Snapshot.Descriptor.RelativePath && a != s.Snapshot.Descriptor {
			return false
		}
		for _, b := range s.Inventory[:i] {
			if a.RootID == b.RootID && a.RelativePath == b.RelativePath {
				return false
			}
		}
	}
	for _, o := range s.Obligations {
		switch o {
		case "placement_unknown", "receipt_pending", "retirement_commit_unknown", "cleanup_pending", "state_reconcile_pending", "lease_close_pending":
		default:
			return false
		}
	}
	switch s.Phase {
	case IntentRecorded:
		return s.RetiredAt.IsZero() && !s.Snapshot.Retired
	case RetirementCommitted, DescriptorCleanupComplete, StateReconciled:
		return !s.RetiredAt.IsZero() && s.Snapshot.Retired
	}
	return false
}

func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

// ValidateReceipt checks only nonsecret schema and operation bindings. It does
// not replace fresh host absence/custody observations during an operation.
func ValidateReceipt(s Receipt) error {
	if !validRequest(s.Request) {
		return errors.New("retirement request invalid")
	}
	raw, _ := json.Marshal(s.Request)
	h := sha256.Sum256(raw)
	check := s.Clone()
	if check.Revision == "" {
		check.Revision = "new"
	}
	if !receiptBound(s.Request, hex.EncodeToString(h[:]), check) {
		return errors.New("retirement receipt invalid")
	}
	if Verify(s.Request, s.Snapshot, s.Proof, s.Proof.ObservedAt).Outcome != Eligible {
		return errors.New("retirement proof binding invalid")
	}
	return nil
}

func payloadCategory(c Category) bool {
	switch c {
	case Descriptor, Staging, Journal, Bridge, HostLog, Sandbox:
		return true
	}
	return false
}
