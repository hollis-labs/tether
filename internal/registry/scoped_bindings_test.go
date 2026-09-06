package registry_test

// scoped_bindings_test.go — T04 acceptance #1/#3 evidence: member
// replacement/rebinding, single-target ambiguity, and binding resolution
// provenance (scope/revision/targets).

import (
	"context"
	"sync"
	"testing"

	"github.com/hollis-labs/tether/internal/registry"
)

func mustRegisterAgent(t *testing.T, svc *registry.Service, displayName string) string {
	t.Helper()
	p, err := svc.Register(context.Background(), registry.KindAgent, registry.Profile{DisplayName: displayName})
	if err != nil {
		t.Fatalf("register %s: %v", displayName, err)
	}
	return p.URN
}

func TestScopedBinding_SetAndResolve(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	alice := mustRegisterAgent(t, svc, "Alice")

	b, err := svc.SetScopedBinding(ctx, "torque:sprint:CW-1", "reviewer", []string{alice}, nil, alice)
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if b.Revision != 1 {
		t.Fatalf("expected first revision to be 1, got %d", b.Revision)
	}

	got, err := svc.ResolveScopedBinding(ctx, "torque:sprint:CW-1", "reviewer")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Revision != 1 || len(got.TargetURNs) != 1 || got.TargetURNs[0] != alice {
		t.Fatalf("unexpected resolution: %+v", got)
	}
}

func TestScopedBinding_MemberReplacementBumpsRevisionAndPreservesHistory(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	alice := mustRegisterAgent(t, svc, "Alice")
	bob := mustRegisterAgent(t, svc, "Bob")

	if _, err := svc.SetScopedBinding(ctx, "team:x:run:1", "engineer", []string{alice}, nil, alice); err != nil {
		t.Fatalf("set rev 1: %v", err)
	}
	// Replacement: bob takes over the slot from alice.
	if _, err := svc.SetScopedBinding(ctx, "team:x:run:1", "engineer", []string{bob}, nil, bob); err != nil {
		t.Fatalf("set rev 2: %v", err)
	}

	current, err := svc.ResolveScopedBinding(ctx, "team:x:run:1", "engineer")
	if err != nil {
		t.Fatalf("resolve current: %v", err)
	}
	if current.Revision != 2 || current.TargetURNs[0] != bob {
		t.Fatalf("expected current binding to be bob at revision 2, got %+v", current)
	}

	history, err := svc.ListScopedBindingRevisions(ctx, "team:x:run:1", "engineer")
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("expected 2 revisions preserved in history, got %d", len(history))
	}
	if history[0].Revision != 2 || history[1].Revision != 1 {
		t.Fatalf("expected newest-first ordering [2,1], got [%d,%d]", history[0].Revision, history[1].Revision)
	}
	if history[1].TargetURNs[0] != alice {
		t.Fatalf("expected revision 1 to remain alice in history (not rewritten), got %+v", history[1])
	}
}

func TestScopedBinding_SingleTargetAmbiguity(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	alice := mustRegisterAgent(t, svc, "Alice")
	bob := mustRegisterAgent(t, svc, "Bob")

	// Zero targets -> ErrBindingHasNoTargets.
	if _, err := svc.SetScopedBinding(ctx, "scope:empty", "slot", nil, nil, alice); err != nil {
		t.Fatalf("set empty: %v", err)
	}
	if _, _, err := svc.ResolveScopedBindingSingle(ctx, "scope:empty", "slot"); err == nil {
		t.Fatalf("expected an error resolving a binding with zero targets")
	}

	// One target -> resolves cleanly.
	if _, err := svc.SetScopedBinding(ctx, "scope:one", "slot", []string{alice}, nil, alice); err != nil {
		t.Fatalf("set one: %v", err)
	}
	single, _, err := svc.ResolveScopedBindingSingle(ctx, "scope:one", "slot")
	if err != nil {
		t.Fatalf("resolve single: %v", err)
	}
	if single != alice {
		t.Fatalf("expected %q, got %q", alice, single)
	}

	// Multiple targets -> ErrAmbiguousBinding, explicit fanout required
	// instead (callers use ResolveScopedBinding directly for that).
	if _, err := svc.SetScopedBinding(ctx, "scope:many", "slot", []string{alice, bob}, nil, alice); err != nil {
		t.Fatalf("set many: %v", err)
	}
	if _, _, err := svc.ResolveScopedBindingSingle(ctx, "scope:many", "slot"); err == nil {
		t.Fatalf("expected ambiguity error resolving a binding with two targets via the single-target accessor")
	}
	fanout, err := svc.ResolveScopedBinding(ctx, "scope:many", "slot")
	if err != nil {
		t.Fatalf("resolve fanout: %v", err)
	}
	if len(fanout.TargetURNs) != 2 {
		t.Fatalf("expected explicit fanout resolution to return both targets, got %+v", fanout.TargetURNs)
	}
}

func TestScopedBinding_Provenance(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()
	alice := mustRegisterAgent(t, svc, "Alice")

	relationship := []byte(`{"reports_to":"` + alice + `"}`)
	b, err := svc.SetScopedBinding(ctx, "team:x:run:1", "engineer", []string{alice}, relationship, alice)
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if b.CreatedBy != alice {
		t.Fatalf("expected provenance created_by=%q, got %q", alice, b.CreatedBy)
	}
	if string(b.Relationship) == "" {
		t.Fatalf("expected relationship metadata to be recorded")
	}
	if b.Scope != "team:x:run:1" || b.Slot != "engineer" {
		t.Fatalf("expected scope/slot to round-trip exactly, got scope=%q slot=%q", b.Scope, b.Slot)
	}
}

func TestScopedBinding_ConcurrentSetsNeverCollideOnRevision(t *testing.T) {
	svc := newServiceForConcurrency(t)
	ctx := context.Background()
	alice := mustRegisterAgent(t, svc, "Alice")

	const n = 8
	revs := make([]int64, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			b, err := svc.SetScopedBinding(ctx, "scope:race", "slot", []string{alice}, nil, alice)
			revs[i] = b.Revision
			errs[i] = err
		}(i)
	}
	wg.Wait()

	seen := map[int64]bool{}
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if seen[revs[i]] {
			t.Fatalf("revision %d claimed by more than one caller", revs[i])
		}
		seen[revs[i]] = true
	}
	if len(seen) != n {
		t.Fatalf("expected %d distinct revisions, got %d: %v", n, len(seen), revs)
	}
}

func TestScopedBinding_Validation(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()

	if _, err := svc.SetScopedBinding(ctx, "", "slot", nil, nil, "someone"); err == nil {
		t.Fatalf("expected error for empty scope")
	}
	if _, err := svc.SetScopedBinding(ctx, "scope", "slot", nil, nil, ""); err == nil {
		t.Fatalf("expected error for empty created_by")
	}
	if _, err := svc.SetScopedBinding(ctx, "scope", "slot", []string{"not-a-urn"}, nil, "someone"); err == nil {
		t.Fatalf("expected error for a malformed target URN")
	}
}
