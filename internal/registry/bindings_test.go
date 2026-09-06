package registry_test

// bindings_test.go — coverage for T02's leased runtime bindings
// (bindings.go / bindings_service.go). Matrix:
//   - LeaseBinding happy path: generation 1, CurrentBinding matches.
//   - A second lease for the same target bumps generation and becomes
//     current; the OLD binding is no longer current but remains resolvable
//     via ListBindingsForTarget (fencing, not deletion).
//   - RenewLease on the current generation succeeds.
//   - RenewLease on a SUPERSEDED (stale) generation fails with
//     ErrStaleGeneration -- this is "one home-host generation per session."
//   - RevokeBinding is idempotent and makes CurrentBinding report
//     ErrBindingNotFound when it was the only binding.
//   - Lease expiry: a binding whose lease_expires_at has passed is not
//     returned by CurrentBinding (reconnect/expiry acceptance case).
//   - Concurrent LeaseBinding calls for the same target never collide on
//     generation number.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/registry"
)

const testTargetURN = "msg://session/agent-mux/sess_abc123"

func TestBindings_LeaseAndCurrent(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()

	b, err := svc.LeaseBinding(ctx, testTargetURN, "sess_abc123", "host-1", "attempt-1", []string{"queue"}, registry.VisibilityPrivateLocal, 0)
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	if b.Generation != 1 {
		t.Fatalf("expected generation 1, got %d", b.Generation)
	}

	cur, err := svc.CurrentBinding(ctx, testTargetURN)
	if err != nil {
		t.Fatalf("current: %v", err)
	}
	if cur.ID != b.ID {
		t.Fatalf("expected current binding to be the one just leased, got %+v", cur)
	}
}

func TestBindings_ReplacementBumpsGenerationAndFencesOld(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()

	first, err := svc.LeaseBinding(ctx, testTargetURN, "sess_abc123", "host-1", "attempt-1", nil, "", 0)
	if err != nil {
		t.Fatalf("lease 1: %v", err)
	}
	second, err := svc.LeaseBinding(ctx, testTargetURN, "sess_abc123", "host-2", "attempt-2", nil, "", 0)
	if err != nil {
		t.Fatalf("lease 2: %v", err)
	}
	if second.Generation != first.Generation+1 {
		t.Fatalf("expected generation to bump: first=%d second=%d", first.Generation, second.Generation)
	}

	cur, err := svc.CurrentBinding(ctx, testTargetURN)
	if err != nil {
		t.Fatalf("current: %v", err)
	}
	if cur.ID != second.ID {
		t.Fatalf("expected the replacement to be current, got %+v", cur)
	}

	// The old binding is fenced (no longer current) but still resolvable
	// for audit/history -- not deleted.
	all, err := svc.ListBindingsForTarget(ctx, testTargetURN)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("expected both bindings to remain listed, got %d", len(all))
	}

	// Renewing the SUPERSEDED generation must fail -- this is the fencing.
	if _, err := svc.RenewBindingLease(ctx, first.ID, time.Minute); err == nil {
		t.Fatalf("expected renewing a stale generation to fail")
	} else if !errors.Is(err, registry.ErrStaleGeneration) {
		t.Fatalf("expected ErrStaleGeneration, got %v", err)
	}

	// Renewing the CURRENT generation must succeed.
	if _, err := svc.RenewBindingLease(ctx, second.ID, time.Minute); err != nil {
		t.Fatalf("expected renewing the current generation to succeed: %v", err)
	}
}

func TestBindings_RevokeIsIdempotentAndClearsCurrent(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()

	b, err := svc.LeaseBinding(ctx, testTargetURN, "sess_abc123", "host-1", "attempt-1", nil, "", 0)
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	if err := svc.RevokeBinding(ctx, b.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	// Idempotent: revoking twice is not an error.
	if err := svc.RevokeBinding(ctx, b.ID); err != nil {
		t.Fatalf("revoke (repeat): %v", err)
	}

	if _, err := svc.CurrentBinding(ctx, testTargetURN); err == nil {
		t.Fatalf("expected no current binding after revoke")
	}

	if err := svc.RevokeBinding(ctx, "nonexistent-binding-id"); err == nil {
		t.Fatalf("expected error revoking an unknown binding id")
	}
}

func TestBindings_ExpiredLeaseIsNotCurrent(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()

	b, err := svc.LeaseBinding(ctx, testTargetURN, "sess_abc123", "host-1", "attempt-1", nil, "", time.Millisecond)
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	time.Sleep(20 * time.Millisecond)

	if _, err := svc.CurrentBinding(ctx, testTargetURN); err == nil {
		t.Fatalf("expected an expired lease to not be current")
	}

	// It remains in the audit list, just not "current".
	all, err := svc.ListBindingsForTarget(ctx, testTargetURN)
	if err != nil || len(all) != 1 || all[0].ID != b.ID {
		t.Fatalf("expected the expired binding to remain listed: %v / %+v", err, all)
	}
}

func TestBindings_ConcurrentLeasesNeverCollideOnGeneration(t *testing.T) {
	svc := newServiceForConcurrency(t)
	ctx := context.Background()

	const n = 8
	gens := make([]int64, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			b, err := svc.LeaseBinding(ctx, testTargetURN, "sess_abc123", "host", "attempt", nil, "", 0)
			gens[i] = b.Generation
			errs[i] = err
		}(i)
	}
	wg.Wait()

	seen := map[int64]bool{}
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if seen[gens[i]] {
			t.Fatalf("generation %d claimed by more than one caller", gens[i])
		}
		seen[gens[i]] = true
	}
	if len(seen) != n {
		t.Fatalf("expected %d distinct generations, got %d: %v", n, len(seen), gens)
	}
}

func TestBindings_Validation(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()

	if _, err := svc.LeaseBinding(ctx, "", "sess", "host", "attempt", nil, "", 0); err == nil {
		t.Fatalf("expected error for empty target_urn")
	}
	if _, err := svc.CurrentBinding(ctx, ""); err == nil {
		t.Fatalf("expected error for empty target_urn on CurrentBinding")
	}
	if err := svc.RevokeBinding(ctx, ""); err == nil {
		t.Fatalf("expected error for empty binding id on RevokeBinding")
	}
}
