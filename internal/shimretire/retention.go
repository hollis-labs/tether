package shimretire

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"time"
)

const RetentionVersion = "shim.retention.v1"

type RetentionMode string

const (
	Keep            RetentionMode = "keep"
	ExplicitBounded RetentionMode = "explicit_bounded"
)

type RetentionPolicy struct {
	Version, ID, Revision              string
	Mode                               RetentionMode
	Categories                         []Category
	RootIDs                            []string
	MinRetiredAge                      *time.Duration
	MaxRetainedBytes, MaxRetainedItems *uint64
	ReplayWaiver                       string
}
type SweepBudget struct {
	MaxExamined, MaxMutated, MaxBytesExamined, MaxBytesRemoved uint64
	MaxDuration                                                time.Duration
}
type SweepRequest struct {
	Version, OperationID, ActorID, AuthorizationID, AuthorizationRevision, ScopeRevision string
	Policy                                                                               RetentionPolicy
	Budget                                                                               SweepBudget
	InspectOnly                                                                          bool
}
type RetentionHold string

const (
	NoRetentionHold      RetentionHold = "clear"
	RetentionHeld        RetentionHold = "held"
	RetentionHoldUnknown RetentionHold = "unknown"
)

type SweepCandidate struct {
	RetirementOperation string
	Hold                RetentionHold
	HoldRevision        string
	Artifact            Artifact
	SortKey             string
}
type InventoryView struct {
	Revision                     string
	RetainedBytes, RetainedItems uint64
}
type CursorPhase string

const (
	CursorReady    CursorPhase = "ready"
	CursorIntent   CursorPhase = "item_intent"
	CursorRemoved  CursorPhase = "item_removed"
	CursorComplete CursorPhase = "complete"
)

// RetainedItem records a bounded durable hold rather than silently skipping a payload.
type RetainedItem struct {
	Candidate SweepCandidate
	Code      string
}

// SweepCursor is control evidence outside all deletable payloads. Pending binds
// exact saved inventory through uncertain cleanup and retry.
type SweepCursor struct {
	Version, ID, RequestDigest, Revision, InventoryRevision, LastKey string
	Request                                                          SweepRequest
	Phase                                                            CursorPhase
	Pending                                                          *SweepCandidate
	Retained                                                         []RetainedItem
}
type CursorStore interface {
	LoadCursor(context.Context, string) (SweepCursor, error)
	RecordCursor(context.Context, SweepCursor, string) (string, error)
}

// RetentionLease owns ALL declared scope/custody/lifecycle/submit/controller
// exclusions before any proof or mutation. Next returns one indexed item and
// does not walk ambient files. Observe uses saved inventory after cleanup.
// Inventory totals include unacknowledged pending items until CursorRemoved is
// durable, including uncertain missing files. This prevents duplicate quota
// accounting when a previous unlink succeeded before cursor persistence.
// Remove must recheck actual named-file identity/context after every callback.
type RetentionLease interface {
	Inventory(context.Context) (InventoryView, error)
	Next(context.Context, string) (SweepCandidate, error)
	Observe(context.Context, SweepCandidate) (SweepCandidate, Snapshot, Observation, error)
	Remove(context.Context, SweepCandidate) (Mutation, error)
	Close() error
}
type SweepPorts struct {
	Cursors  CursorStore
	Receipts Store
	Acquire  func(context.Context, SweepRequest) (RetentionLease, error)
	Validate func(context.Context, SweepRequest) error
	Now      func() time.Time
}
type SweepOutcome string

const (
	SweepRetainedUnknown  SweepOutcome = "retained_unknown"
	SweepRetainedByPolicy SweepOutcome = "retained_by_policy"
	SweepComplete         SweepOutcome = "complete"
	SweepBounded          SweepOutcome = "bounded"
	SweepUnsupported      SweepOutcome = "unsupported"
)

type SweepResult struct {
	Outcome                                        SweepOutcome
	Code, CursorID                                 string
	Examined, Mutated, BytesExamined, BytesRemoved uint64
	Obligations                                    []string
	Retained                                       []RetainedItem
}

func retentionDigest(r SweepRequest) string {
	b, _ := json.Marshal(r)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func (r SweepRequest) Clone() SweepRequest {
	r.Policy.Categories = slices.Clone(r.Policy.Categories)
	r.Policy.RootIDs = slices.Clone(r.Policy.RootIDs)
	if p := r.Policy.MinRetiredAge; p != nil {
		v := *p
		r.Policy.MinRetiredAge = &v
	}
	if p := r.Policy.MaxRetainedBytes; p != nil {
		v := *p
		r.Policy.MaxRetainedBytes = &v
	}
	if p := r.Policy.MaxRetainedItems; p != nil {
		v := *p
		r.Policy.MaxRetainedItems = &v
	}
	return r
}
func (c SweepCursor) Clone() SweepCursor {
	c.Request = c.Request.Clone()
	c.Retained = slices.Clone(c.Retained)
	if c.Pending != nil {
		v := *c.Pending
		c.Pending = &v
	}
	return c
}
func ValidateSweepRequest(r SweepRequest) error {
	p := r.Policy
	b := r.Budget
	if r.Version != RetentionVersion || p.Version != RetentionVersion || !text(r.OperationID, r.ActorID, r.AuthorizationID, r.AuthorizationRevision, r.ScopeRevision, p.ID, p.Revision) || p.Mode != ExplicitBounded || len(p.Categories) == 0 || len(p.Categories) > 6 || len(p.RootIDs) == 0 || len(p.RootIDs) > 256 || b.MaxExamined == 0 || b.MaxExamined > 1024 || b.MaxMutated == 0 || b.MaxMutated > 256 || b.MaxBytesExamined == 0 || b.MaxBytesExamined > 1<<40 || b.MaxBytesRemoved == 0 || b.MaxBytesRemoved > 1<<40 || b.MaxDuration <= 0 || b.MaxDuration > time.Minute {
		return errors.New("retention request invalid")
	}
	if p.MinRetiredAge == nil && p.MaxRetainedBytes == nil && p.MaxRetainedItems == nil {
		return errors.New("explicit retention criterion required")
	}
	if p.MinRetiredAge != nil && *p.MinRetiredAge < 0 || p.MaxRetainedBytes != nil && *p.MaxRetainedBytes == 0 || p.MaxRetainedItems != nil && *p.MaxRetainedItems == 0 {
		return errors.New("retention criterion invalid")
	}
	for i, c := range p.Categories {
		if !payloadCategory(c) || c == Descriptor || slices.Contains(p.Categories[:i], c) {
			return errors.New("retention category invalid")
		}
	}
	for i, id := range p.RootIDs {
		if !text(id) || slices.Contains(p.RootIDs[:i], id) {
			return errors.New("retention scope invalid")
		}
	}
	return nil
}
func ValidateSweepCursor(c SweepCursor) error {
	if ValidateSweepRequest(c.Request) != nil || c.Version != RetentionVersion || c.ID != c.Request.OperationID || c.RequestDigest != retentionDigest(c.Request) || !text(c.InventoryRevision) || (c.LastKey != "" && !text(c.LastKey)) {
		return errors.New("retention cursor invalid")
	}
	if len(c.Retained) > 256 {
		return errors.New("retention cursor holds unbounded")
	}
	for _, item := range c.Retained {
		if !validSweepCandidate(item.Candidate) || item.Candidate.SortKey > c.LastKey {
			return errors.New("retention cursor hold invalid")
		}
		switch item.Code {
		case "retention_hold", "inspect_only", "item_exceeds_budget":
		default:
			return errors.New("retention cursor hold code invalid")
		}
	}
	switch c.Phase {
	case CursorReady, CursorComplete:
		if c.Pending != nil {
			return errors.New("retention cursor pending mismatch")
		}
	case CursorIntent, CursorRemoved:
		if c.Pending == nil || !validSweepCandidate(*c.Pending) || c.Pending.SortKey <= c.LastKey {
			return errors.New("retention cursor item invalid")
		}
	default:
		return errors.New("retention cursor phase invalid")
	}
	return nil
}
func validSweepCandidate(c SweepCandidate) bool {
	a := c.Artifact
	return text(c.RetirementOperation, c.SortKey, a.RootID, a.OwnerID, a.CustodyRevision, a.InventoryRevision) && relative(a.RelativePath) && a.Identity.Kind == "regular" && a.Identity.Inode != 0 && payloadCategory(a.Category) && a.Category != Descriptor
}
func Sweep(ctx context.Context, request SweepRequest, p SweepPorts) (out SweepResult) {
	r := request.Clone()
	out.CursorID = r.OperationID
	defer func() {
		if (out.Outcome == SweepRetainedUnknown || out.Outcome == SweepUnsupported || out.Outcome == SweepRetainedByPolicy) && !slices.Contains(out.Obligations, "retention_hold") {
			out.Obligations = append(out.Obligations, "retention_hold")
		}
	}()
	// Default KEEP requires no adapter, cursor, ambient observation or effect.
	if r.Policy.Mode == "" || r.Policy.Mode == Keep {
		out.Outcome = "retained_by_policy"
		out.Code = "keep"
		return
	}
	out.Outcome = "retained_unknown"
	out.Code = "retention_unavailable"
	if ValidateSweepRequest(r) != nil {
		out.Code = "request_refused"
		return
	}
	if p.Cursors == nil || p.Receipts == nil || p.Acquire == nil || p.Validate == nil || p.Now == nil {
		out.Outcome = SweepUnsupported
		out.Code = "proof_adapter_unavailable"
		return
	}
	run, cancel := context.WithTimeout(ctx, r.Budget.MaxDuration)
	defer cancel()
	if run.Err() != nil || p.Validate(run, r) != nil || run.Err() != nil {
		out.Code = "authority_refused"
		return
	}
	lease, err := p.Acquire(run, r.Clone())
	if err != nil || lease == nil {
		return
	}
	defer func() {
		if lease.Close() != nil {
			out.Obligations = append(out.Obligations, "lease_close_pending")
			out.Outcome = "retained_unknown"
		}
	}()
	view, err := lease.Inventory(run)
	if err != nil || !text(view.Revision) || run.Err() != nil {
		return
	}
	cursor, err := p.Cursors.LoadCursor(run, r.OperationID)
	if errors.Is(err, ErrNotFound) {
		cursor = SweepCursor{Version: RetentionVersion, ID: r.OperationID, Request: r.Clone(), RequestDigest: retentionDigest(r), InventoryRevision: view.Revision, Phase: CursorReady}
	} else if err != nil {
		out.Code = "cursor_unavailable"
		return
	}
	if ValidateSweepCursor(cursor) == nil {
		defer func() {
			out.Retained = slices.Clone(cursor.Retained)
			if len(cursor.Retained) > 0 && !slices.Contains(out.Obligations, "retention_hold") {
				out.Obligations = append(out.Obligations, "retention_hold")
			}
			if cursor.Pending != nil && !slices.Contains(out.Obligations, "retention_pending") {
				out.Obligations = append(out.Obligations, "retention_pending")
			}
		}()
	}
	if ValidateSweepCursor(cursor) != nil || cursor.RequestDigest != retentionDigest(r) || cursor.InventoryRevision != view.Revision {
		out.Code = "cursor_refused"
		return
	}
	record := func() bool {
		rev, e := p.Cursors.RecordCursor(run, cursor.Clone(), cursor.Revision)
		if e != nil || rev == "" || run.Err() != nil {
			out.Code = "cursor_pending"
			out.Obligations = append(out.Obligations, "cursor_pending")
			return false
		}
		cursor.Revision = rev
		return true
	}
	retain := func(candidate SweepCandidate, code string) bool {
		if cursor.Pending != nil || len(cursor.Retained) >= 256 {
			out.Code = "retained_pressure"
			out.Obligations = append(out.Obligations, "retention_hold")
			return false
		}
		cursor.Retained = append(cursor.Retained, RetainedItem{candidate, code})
		cursor.LastKey = candidate.SortKey
		cursor.Phase = CursorReady
		return record()
	}
	if cursor.Revision == "" && !record() {
		return
	}
	if cursor.Phase == CursorComplete {
		out.Outcome = "complete"
		out.Code = "already_complete"
		return
	}
	for out.Examined < r.Budget.MaxExamined {
		if run.Err() != nil {
			out.Code = "budget_exhausted"
			return
		}
		if cursor.Phase == CursorRemoved {
			cursor.LastKey = cursor.Pending.SortKey
			cursor.Pending = nil
			cursor.Phase = CursorReady
			if !record() {
				return
			}
		}
		var candidate SweepCandidate
		if cursor.Pending != nil {
			candidate = *cursor.Pending
		} else {
			candidate, err = lease.Next(run, cursor.LastKey)
			if errors.Is(err, ErrNotFound) {
				cursor.Phase = CursorComplete
				if !record() {
					return
				}
				out.Outcome = "complete"
				out.Code = "inventory_complete"
				return
			}
			if err != nil {
				return
			}
		}
		if !validSweepCandidate(candidate) || candidate.SortKey <= cursor.LastKey {
			out.Code = "inventory_refused"
			return
		}
		size := candidate.Artifact.Size
		if size > r.Budget.MaxBytesExamined-out.BytesExamined {
			out.Examined++
			out.Obligations = append(out.Obligations, "item_exceeds_budget")
			if !retain(candidate, "item_exceeds_budget") {
				return
			}
			continue
		}
		out.Examined++
		out.BytesExamined += size
		receipt, e := p.Receipts.Load(run, candidate.RetirementOperation)
		eligible := e == nil && ValidateReceipt(receipt) == nil && receipt.Phase == StateReconciled && len(receipt.Obligations) == 0 && slices.Contains(receipt.Inventory, candidate.Artifact) && slices.Contains(r.Policy.Categories, candidate.Artifact.Category) && slices.Contains(r.Policy.RootIDs, candidate.Artifact.RootID) && !receipt.RetiredAt.After(p.Now()) && candidate.Hold == NoRetentionHold && text(candidate.HoldRevision)
		if candidate.Artifact.Category == Journal || candidate.Artifact.Category == Bridge {
			eligible = eligible && text(r.Policy.ReplayWaiver)
		}
		ageEligible := r.Policy.MinRetiredAge != nil && p.Now().Sub(receipt.RetiredAt) >= *r.Policy.MinRetiredAge
		overQuota := r.Policy.MaxRetainedBytes != nil && view.RetainedBytes > *r.Policy.MaxRetainedBytes || r.Policy.MaxRetainedItems != nil && view.RetainedItems > *r.Policy.MaxRetainedItems
		hasQuota := r.Policy.MaxRetainedBytes != nil || r.Policy.MaxRetainedItems != nil
		ageFloor := r.Policy.MinRetiredAge == nil || ageEligible
		selected := overQuota || (!hasQuota && ageEligible)
		eligible = eligible && ageFloor && selected
		if cursor.Pending != nil && !eligible {
			out.Code = "pending_item_retained"
			out.Obligations = append(out.Obligations, "retention_pending")
			return
		}
		if !eligible || r.InspectOnly {
			code := "retention_hold"
			if r.InspectOnly {
				code = "inspect_only"
			}
			if !retain(candidate, code) {
				return
			}
			continue
		}
		if out.Mutated >= r.Budget.MaxMutated || size > r.Budget.MaxBytesRemoved-out.BytesRemoved {
			if out.Mutated >= r.Budget.MaxMutated {
				out.Code = "mutation_budget_exhausted"
				return
			}
			out.Obligations = append(out.Obligations, "item_exceeds_budget")
			if !retain(candidate, "item_exceeds_budget") {
				return
			}
			continue
		}
		if cursor.Pending == nil {
			cursor.Pending = &candidate
			cursor.Phase = CursorIntent
			if !record() {
				return
			}
		}
		if run.Err() != nil || p.Validate(run, r.Clone()) != nil || run.Err() != nil {
			out.Code = "authority_refused"
			out.Obligations = append(out.Obligations, "retention_pending")
			return
		}
		// Actual observation follows the durable cursor and authority callbacks.
		// Refresh the durable audit after cursor callbacks before the final
		// authority/resource observation; callbacks cannot hide new obligations.
		receipt, e = p.Receipts.Load(run, candidate.RetirementOperation)
		if e != nil || ValidateReceipt(receipt) != nil || receipt.Phase != StateReconciled || len(receipt.Obligations) != 0 || !slices.Contains(receipt.Inventory, candidate.Artifact) {
			out.Code = "retirement_receipt_changed"
			out.Obligations = append(out.Obligations, "retention_pending")
			return
		}
		if view.RetainedBytes < size || view.RetainedItems == 0 {
			out.Code = "inventory_accounting_unknown"
			out.Obligations = append(out.Obligations, "retention_pending")
			return
		}
		if run.Err() != nil || p.Validate(run, r.Clone()) != nil || run.Err() != nil {
			out.Code = "authority_refused"
			out.Obligations = append(out.Obligations, "retention_pending")
			return
		}
		observed, snapshot, proof, e := lease.Observe(run, candidate)
		if e != nil || run.Err() != nil || observed != candidate || observed.Hold != NoRetentionHold || snapshot != receipt.Snapshot || Verify(receipt.Request, snapshot, proof, p.Now()).Outcome != Eligible {
			out.Code = "item_custody_refused"
			out.Obligations = append(out.Obligations, "retention_pending")
			return
		}
		if run.Err() != nil || p.Validate(run, r.Clone()) != nil || run.Err() != nil {
			out.Code = "authority_refused"
			out.Obligations = append(out.Obligations, "retention_pending")
			return
		}
		mutation, e := lease.Remove(run, candidate)
		if e != nil || run.Err() != nil || (mutation != Changed && mutation != NoChange) {
			out.Code = "retention_uncertain"
			out.Obligations = append(out.Obligations, "retention_pending")
			return
		}
		if mutation == Changed {
			out.Mutated++
			out.BytesRemoved += size
		}
		if view.RetainedBytes < size || view.RetainedItems == 0 {
			out.Code = "inventory_accounting_unknown"
			out.Obligations = append(out.Obligations, "retention_pending")
			return
		}
		view.RetainedBytes -= size
		view.RetainedItems--
		cursor.Phase = CursorRemoved
		if !record() {
			return
		}
	}
	out.Outcome = "bounded"
	out.Code = "item_budget_exhausted"
	return
}
