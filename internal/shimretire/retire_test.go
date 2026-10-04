package shimretire

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

type memoryStore struct {
	receipt    Receipt
	events     *[]string
	failPhase  Phase
	recordHook func(Receipt)
}

func (s *memoryStore) Load(context.Context, string) (Receipt, error) {
	if s.receipt.OperationID == "" {
		return Receipt{}, ErrNotFound
	}
	return s.receipt.Clone(), nil
}
func (s *memoryStore) Record(_ context.Context, r Receipt, expected string) (string, error) {
	if expected != s.receipt.Revision {
		return "", errors.New("revision changed")
	}
	*s.events = append(*s.events, "record:"+string(r.Phase))
	if s.recordHook != nil {
		s.recordHook(r)
	}
	if r.Phase == s.failPhase {
		return "", errors.New("persist failed")
	}
	r.Revision = s.receipt.Revision + "r"
	s.receipt = r.Clone()
	return r.Revision, nil
}

type fakeLease struct {
	snapshot                        Snapshot
	proof                           Observation
	events                          *[]string
	commitErr, cleanupErr, stateErr error
	commitMutation                  Mutation
	observations                    int
	observeHook                     func(*fakeLease)
	closed                          bool
}

func (l *fakeLease) Observe(context.Context) (Snapshot, Observation, error) {
	l.observations++
	if l.observeHook != nil {
		l.observeHook(l)
	}
	return l.snapshot, l.proof, nil
}
func (l *fakeLease) CommitRetired(_ context.Context, s Snapshot) (Mutation, error) {
	*l.events = append(*l.events, "commit")
	if s != l.snapshot {
		return NoChange, errors.New("stale snapshot")
	}
	if l.commitErr != nil {
		return l.commitMutation, l.commitErr
	}
	l.snapshot.Retired = true
	return Changed, nil
}
func (l *fakeLease) CleanupDescriptor(context.Context, Artifact) (Mutation, error) {
	*l.events = append(*l.events, "cleanup")
	if l.cleanupErr != nil {
		return Uncertain, l.cleanupErr
	}
	return Changed, nil
}
func (l *fakeLease) ReconcileState(context.Context, Placement, string) error {
	*l.events = append(*l.events, "state")
	return l.stateErr
}
func (l *fakeLease) Close() error { l.closed = true; return nil }
func retireFixture() (Request, Ports, *memoryStore, *fakeLease, *[]string) {
	r, s, o, now := validProof()
	events := []string{}
	store := &memoryStore{events: &events}
	lease := &fakeLease{snapshot: s, proof: o, events: &events}
	p := Ports{Store: store, Acquire: func(context.Context, Placement) (Lease, error) { return lease, nil }, Validate: func(context.Context, Request) error { return nil }, Now: func() time.Time { return now }}
	return r, p, store, lease, &events
}

func TestRetirementDurableOrderAndRetry(t *testing.T) {
	r, p, s, l, events := retireFixture()
	got := RetireAbsent(context.Background(), r, p)
	if got.Outcome != Retired {
		t.Fatalf("retire: %+v", got)
	}
	want := []string{"record:intent_recorded", "commit", "record:retirement_committed", "cleanup", "record:descriptor_cleanup_complete", "state", "record:state_reconciled"}
	if !slices.Equal(*events, want) || s.receipt.Phase != StateReconciled || !l.closed {
		t.Fatalf("order: %v receipt:%+v", *events, s.receipt)
	}
	*events = nil
	got = RetireAbsent(context.Background(), r, p)
	if got.Outcome != AlreadyRetired || len(*events) != 0 {
		t.Fatalf("retry: %+v events:%v", got, *events)
	}
}
func TestUnknownCannotMutate(t *testing.T) {
	r, p, _, l, events := retireFixture()
	l.proof.Descendants = ExecutionUnknown
	got := RetireAbsent(context.Background(), r, p)
	if got.Outcome == Retired || len(*events) != 0 || len(got.Obligations) == 0 {
		t.Fatalf("unknown: %+v %v", got, *events)
	}
}
func TestIntentCallbackRequiresFreshProof(t *testing.T) {
	r, p, s, l, events := retireFixture()
	s.recordHook = func(r Receipt) {
		if r.Phase == IntentRecorded {
			l.proof.Descendants = Present
		}
	}
	got := RetireAbsent(context.Background(), r, p)
	if got.Outcome != RefusedPossibleChild || slices.Contains(*events, "commit") {
		t.Fatalf("late child: %+v %v", got, *events)
	}
}
func TestRetirementCommitFailureNeverCleans(t *testing.T) {
	for _, failure := range []string{"intent", "commit", "receipt"} {
		t.Run(failure, func(t *testing.T) {
			r, p, s, l, events := retireFixture()
			switch failure {
			case "intent":
				s.failPhase = IntentRecorded
			case "commit":
				l.commitErr = errors.New("unknown")
				l.commitMutation = Uncertain
			case "receipt":
				s.failPhase = RetirementCommitted
			}
			got := RetireAbsent(context.Background(), r, p)
			if got.Outcome == Retired || slices.Contains(*events, "cleanup") || len(got.Obligations) == 0 {
				t.Fatalf("unsafe: %+v %v", got, *events)
			}
		})
	}
}
func TestCleanupPendingRetry(t *testing.T) {
	r, p, s, l, events := retireFixture()
	l.cleanupErr = errors.New("swapped")
	got := RetireAbsent(context.Background(), r, p)
	if got.Outcome != RetiredCleanupPending || s.receipt.Phase != RetirementCommitted || slices.Contains(*events, "state") {
		t.Fatalf("cleanup failure: %+v %v", got, *events)
	}
	l.cleanupErr = nil
	*events = nil
	got = RetireAbsent(context.Background(), r, p)
	if got.Outcome != Retired || slices.Contains(*events, "commit") {
		t.Fatalf("cleanup retry: %+v %v", got, *events)
	}
}
