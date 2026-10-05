package shimretire

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

type memoryCursors struct {
	cursor SweepCursor
	hook   func(SweepCursor)
	fail   CursorPhase
}

func (s *memoryCursors) LoadCursor(context.Context, string) (SweepCursor, error) {
	if s.cursor.ID == "" {
		return SweepCursor{}, ErrNotFound
	}
	return s.cursor.Clone(), nil
}
func (s *memoryCursors) RecordCursor(_ context.Context, c SweepCursor, expected string) (string, error) {
	if expected != s.cursor.Revision {
		return "", errors.New("stale")
	}
	if s.hook != nil {
		s.hook(c)
	}
	if c.Phase == s.fail {
		return "", errors.New("persist failed")
	}
	c.Revision = s.cursor.Revision + "r"
	s.cursor = c.Clone()
	return c.Revision, nil
}

type retentionFixtureLease struct {
	candidate   SweepCandidate
	snapshot    Snapshot
	proof       Observation
	view        InventoryView
	removes     int
	removeErr   error
	observeHook func(*retentionFixtureLease)
	removed     bool
}

func (l *retentionFixtureLease) Inventory(context.Context) (InventoryView, error) { return l.view, nil }
func (l *retentionFixtureLease) Next(_ context.Context, key string) (SweepCandidate, error) {
	if key >= l.candidate.SortKey {
		return SweepCandidate{}, ErrNotFound
	}
	return l.candidate, nil
}
func (l *retentionFixtureLease) Observe(context.Context, SweepCandidate) (SweepCandidate, Snapshot, Observation, error) {
	if l.observeHook != nil {
		l.observeHook(l)
	}
	return l.candidate, l.snapshot, l.proof, nil
}
func (l *retentionFixtureLease) Remove(context.Context, SweepCandidate) (Mutation, error) {
	l.removes++
	if l.removeErr != nil {
		return Uncertain, l.removeErr
	}
	if l.removed {
		return NoChange, nil
	}
	l.removed = true
	return Changed, nil
}
func (l *retentionFixtureLease) Close() error { return nil }
func retentionFixture() (SweepRequest, SweepPorts, *memoryCursors, *memoryStore, *retentionFixtureLease) {
	r, s, o, now := validProof()
	s.Retired = true
	a := s.Descriptor
	a.Category = HostLog
	a.RelativePath = "host.log"
	a.Size = 7
	receipt := Receipt{Version: Version, OperationID: r.OperationID, Request: r, RequestDigest: retirementRequestDigest(r), Revision: "retired", Snapshot: s, Proof: o, Phase: StateReconciled, RetiredAt: now.Add(-time.Hour), Inventory: []Artifact{a}}
	events := []string{}
	store := &memoryStore{receipt: receipt, events: &events}
	cursors := &memoryCursors{}
	age := time.Minute
	request := SweepRequest{Version: RetentionVersion, OperationID: "sweep", ActorID: "operator", AuthorizationID: "grant", AuthorizationRevision: "grant-revision", ScopeRevision: "scope", Policy: RetentionPolicy{Version: RetentionVersion, ID: "explicit", Revision: "policy", Mode: ExplicitBounded, Categories: []Category{HostLog}, RootIDs: []string{a.RootID}, MinRetiredAge: &age}, Budget: SweepBudget{MaxExamined: 10, MaxMutated: 2, MaxBytesExamined: 100, MaxBytesRemoved: 100, MaxDuration: time.Second}}
	lease := &retentionFixtureLease{candidate: SweepCandidate{RetirementOperation: r.OperationID, Artifact: a, SortKey: "retired/session/host.log", Hold: NoRetentionHold, HoldRevision: "holds"}, snapshot: s, proof: o, view: InventoryView{Revision: "inventory", RetainedBytes: 7, RetainedItems: 1}}
	ports := SweepPorts{Cursors: cursors, Receipts: store, Acquire: func(context.Context, SweepRequest) (RetentionLease, error) { return lease, nil }, Validate: func(context.Context, SweepRequest) error { return nil }, Now: func() time.Time { return now }}
	return request, ports, cursors, store, lease
}
func TestRetentionKeepHasNoEffects(t *testing.T) {
	for _, mode := range []RetentionMode{"", Keep} {
		r, _, _, _, _ := retentionFixture()
		r.Policy.Mode = mode
		out := Sweep(context.Background(), r, SweepPorts{})
		if out.Outcome != "retained_by_policy" || out.Examined != 0 || out.Mutated != 0 {
			t.Fatalf("KEEP: %+v", out)
		}
	}
}
func TestRetentionBoundedRemovalAndRetry(t *testing.T) {
	r, p, c, _, l := retentionFixture()
	out := Sweep(context.Background(), r, p)
	if out.Outcome != "complete" || out.Mutated != 1 || out.BytesRemoved != 7 || c.cursor.Phase != CursorComplete || l.removes != 1 {
		t.Fatalf("sweep: %+v cursor:%+v removes:%d", out, c.cursor, l.removes)
	}
	out = Sweep(context.Background(), r, p)
	if out.Outcome != "complete" || l.removes != 1 {
		t.Fatalf("repeat: %+v removes:%d", out, l.removes)
	}
}
func TestRetentionRefusesUnprovenOrHeldPayloads(t *testing.T) {
	for _, name := range []string{"pending", "hold", "hold-unknown", "foreign", "replay", "active", "unknown-proof", "cleanup-incomplete"} {
		t.Run(name, func(t *testing.T) {
			r, p, c, s, l := retentionFixture()
			switch name {
			case "pending":
				s.receipt.Obligations = []string{"cleanup_pending"}
			case "hold":
				l.candidate.Hold = RetentionHeld
			case "hold-unknown":
				l.candidate.Hold = ""
			case "foreign":
				l.candidate.Artifact.Identity.Inode++
			case "replay":
				l.candidate.Artifact.Category = Bridge
				s.receipt.Inventory = []Artifact{l.candidate.Artifact}
				r.Policy.Categories = []Category{Bridge}
			case "cleanup-incomplete":
				s.receipt.Phase = DescriptorCleanupComplete
			case "active":
				s.receipt.Phase = IntentRecorded
				s.receipt.Snapshot.Retired = false
				s.receipt.RetiredAt = time.Time{}
			case "unknown-proof":
				l.proof.Descendants = ExecutionUnknown
			}
			out := Sweep(context.Background(), r, p)
			if l.removes != 0 || out.Mutated != 0 {
				t.Fatalf("unsafe %s: %+v removes:%d", name, out, l.removes)
			}
			if name != "unknown-proof" && c.cursor.Pending != nil {
				t.Fatalf("ineligible item acquired durable removal intent: %s", name)
			}
		})
	}
}
func TestRetentionRechecksAfterCursorCallback(t *testing.T) {
	for _, name := range []string{"descendant", "hold", "audit"} {
		t.Run(name, func(t *testing.T) {
			r, p, c, s, l := retentionFixture()
			c.hook = func(cursor SweepCursor) {
				if cursor.Phase == CursorIntent {
					switch name {
					case "descendant":
						l.proof.Descendants = Present
					case "hold":
						l.candidate.Hold = RetentionHeld
					case "audit":
						s.receipt.Obligations = []string{"cleanup_pending"}
					}
				}
			}
			out := Sweep(context.Background(), r, p)
			if l.removes != 0 || len(out.Obligations) == 0 || c.cursor.Phase != CursorIntent {
				t.Fatalf("late %s: %+v removes:%d cursor:%s", name, out, l.removes, c.cursor.Phase)
			}
		})
	}
}
func TestRetentionUncertainRemovalRetainsCursor(t *testing.T) {
	r, p, c, _, l := retentionFixture()
	l.removeErr = errors.New("uncertain")
	out := Sweep(context.Background(), r, p)
	if c.cursor.Phase != CursorIntent || c.cursor.Pending == nil || len(out.Obligations) == 0 {
		t.Fatalf("uncertain: %+v cursor:%+v", out, c.cursor)
	}
	l.removeErr = nil
	l.removed = true
	out = Sweep(context.Background(), r, p)
	if out.Outcome != "complete" || out.Mutated != 0 || c.cursor.Phase != CursorComplete {
		t.Fatalf("missing-file retry: %+v cursor:%+v", out, c.cursor)
	}
}
func TestRetentionBudgetAndCursorBinding(t *testing.T) {
	for _, name := range []string{"bytes", "mutation", "inventory", "request"} {
		t.Run(name, func(t *testing.T) {
			r, p, c, _, l := retentionFixture()
			switch name {
			case "bytes":
				r.Budget.MaxBytesExamined = 1
			case "mutation":
				r.Budget.MaxBytesRemoved = 1
			case "inventory", "request":
				c.cursor = SweepCursor{Version: RetentionVersion, ID: r.OperationID, RequestDigest: retentionDigest(r), Request: r, InventoryRevision: l.view.Revision, Revision: "cursor", Phase: CursorReady}
				if name == "inventory" {
					l.view.Revision = "changed"
				} else {
					r.ActorID = "different"
				}
			}
			out := Sweep(context.Background(), r, p)
			if l.removes != 0 || out.Mutated != 0 {
				t.Fatalf("budget/binding %s: %+v", name, out)
			}
		})
	}
}

func retirementRequestDigest(r Request) string {
	raw, _ := json.Marshal(r)
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:])
}

func TestRetentionQuotaHonorsMinimumAge(t *testing.T) {
	for _, name := range []string{"over-bytes", "below-bytes", "young-over-bytes", "over-items"} {
		t.Run(name, func(t *testing.T) {
			r, p, _, s, l := retentionFixture()
			bytes := uint64(6)
			items := uint64(1)
			switch name {
			case "over-bytes":
				r.Policy.MaxRetainedBytes = &bytes
			case "below-bytes":
				bytes = 8
				r.Policy.MaxRetainedBytes = &bytes
			case "young-over-bytes":
				r.Policy.MaxRetainedBytes = &bytes
				s.receipt.RetiredAt = p.Now()
			case "over-items":
				r.Policy.MaxRetainedItems = &items
				l.view.RetainedItems = 2
			}
			out := Sweep(context.Background(), r, p)
			want := uint64(1)
			if name == "below-bytes" || name == "young-over-bytes" {
				want = 0
			}
			if out.Mutated != want || uint64(l.removes) != want {
				t.Fatalf("quota %s: %+v removes:%d", name, out, l.removes)
			}
		})
	}
}

func TestRetentionOversizeItemAdvancesWithoutMutation(t *testing.T) {
	r, p, c, _, l := retentionFixture()
	r.Budget.MaxBytesExamined = 1
	out := Sweep(context.Background(), r, p)
	if l.removes != 0 || c.cursor.LastKey != l.candidate.SortKey || len(out.Obligations) == 0 {
		t.Fatalf("oversize item loses bounded progress: %+v cursor:%+v removes:%d", out, c.cursor, l.removes)
	}
}

func TestRetentionRechecksAuthorityAfterObservation(t *testing.T) {
	r, p, _, _, l := retentionFixture()
	revoked := false
	p.Validate = func(context.Context, SweepRequest) error {
		if revoked {
			return errors.New("revoked")
		}
		return nil
	}
	l.observeHook = func(*retentionFixtureLease) { revoked = true }
	out := Sweep(context.Background(), r, p)
	if l.removes != 0 || len(out.Obligations) == 0 {
		t.Fatalf("observation revocation ignored: %+v removes:%d", out, l.removes)
	}
}
