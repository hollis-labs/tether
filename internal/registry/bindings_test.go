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

// TestBindings_LeaseUnlessVisibility_Matrix covers the T07 guarded-lease
// primitive's decision table directly at the Storage/Service level (the
// HTTP-layer tests in internal/api/bindings_test.go exercise the same
// method through the handler; this is the unit-level proof for each
// distinct starting state).
func TestBindings_LeaseUnlessVisibility_Matrix(t *testing.T) {
	blocked := []registry.PublicationVisibility{registry.VisibilityPrivateLocal, registry.VisibilityTetherHosted}

	t.Run("never bound succeeds", func(t *testing.T) {
		svc := newService(t)
		ctx := context.Background()
		b, err := svc.LeaseBindingUnlessVisibility(ctx, testTargetURN, "bridge-1", "host", "attempt", []string{"pull-only"}, registry.VisibilityPublishedLocal, 0, blocked...)
		if err != nil {
			t.Fatalf("lease: %v", err)
		}
		if b.Visibility != registry.VisibilityPublishedLocal {
			t.Fatalf("visibility = %q, want published-local", b.Visibility)
		}
	})

	t.Run("current private-local refuses", func(t *testing.T) {
		svc := newService(t)
		ctx := context.Background()
		if _, err := svc.LeaseBinding(ctx, testTargetURN, "real-session", "local", "real-session", nil, registry.VisibilityPrivateLocal, 0); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if _, err := svc.LeaseBindingUnlessVisibility(ctx, testTargetURN, "bridge-1", "host", "attempt", []string{"pull-only"}, registry.VisibilityPublishedLocal, 0, blocked...); !errors.Is(err, registry.ErrVisibilityConflict) {
			t.Fatalf("expected ErrVisibilityConflict, got %v", err)
		}
	})

	t.Run("current tether-hosted refuses", func(t *testing.T) {
		svc := newService(t)
		ctx := context.Background()
		if _, err := svc.LeaseBinding(ctx, testTargetURN, "real-session", "local", "real-session", nil, registry.VisibilityTetherHosted, 0); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if _, err := svc.LeaseBindingUnlessVisibility(ctx, testTargetURN, "bridge-1", "host", "attempt", []string{"pull-only"}, registry.VisibilityPublishedLocal, 0, blocked...); !errors.Is(err, registry.ErrVisibilityConflict) {
			t.Fatalf("expected ErrVisibilityConflict, got %v", err)
		}
	})

	t.Run("current published-local supersedes", func(t *testing.T) {
		svc := newService(t)
		ctx := context.Background()
		first, err := svc.LeaseBindingUnlessVisibility(ctx, testTargetURN, "bridge-1", "host", "attempt", []string{"pull-only"}, registry.VisibilityPublishedLocal, 0, blocked...)
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		second, err := svc.LeaseBindingUnlessVisibility(ctx, testTargetURN, "bridge-2", "host", "attempt", []string{"pull-only"}, registry.VisibilityPublishedLocal, 0, blocked...)
		if err != nil {
			t.Fatalf("expected supersede of an existing published-local binding to succeed: %v", err)
		}
		if second.Generation != first.Generation+1 {
			t.Fatalf("generation = %d, want %d", second.Generation, first.Generation+1)
		}
	})

	t.Run("revoked private-local no longer blocks", func(t *testing.T) {
		svc := newService(t)
		ctx := context.Background()
		revoked, err := svc.LeaseBinding(ctx, testTargetURN, "real-session", "local", "real-session", nil, registry.VisibilityPrivateLocal, 0)
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		if err := svc.RevokeBinding(ctx, revoked.ID); err != nil {
			t.Fatalf("revoke: %v", err)
		}
		if _, err := svc.LeaseBindingUnlessVisibility(ctx, testTargetURN, "bridge-1", "host", "attempt", []string{"pull-only"}, registry.VisibilityPublishedLocal, 0, blocked...); err != nil {
			t.Fatalf("expected lease over a revoked private-local binding to succeed: %v", err)
		}
	})

	t.Run("expired private-local no longer blocks", func(t *testing.T) {
		svc := newService(t)
		ctx := context.Background()
		if _, err := svc.LeaseBinding(ctx, testTargetURN, "real-session", "local", "real-session", nil, registry.VisibilityPrivateLocal, time.Millisecond); err != nil {
			t.Fatalf("seed: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
		if _, err := svc.LeaseBindingUnlessVisibility(ctx, testTargetURN, "bridge-1", "host", "attempt", []string{"pull-only"}, registry.VisibilityPublishedLocal, 0, blocked...); err != nil {
			t.Fatalf("expected lease over an expired private-local binding to succeed: %v", err)
		}
	})
}

// TestBindings_LeaseUnlessVisibility_ConcurrentRaceNeverInterleaves is the
// concurrency proof a distinct review pass found missing: the original
// internal/api/bindings.go implementation did its "is the current binding
// Tether-managed" check and its LeaseBinding mint as two SEPARATE calls
// (two separate connection acquisitions against the MaxOpenConns=1 pool),
// leaving a real window for a concurrent private-local lease to land in
// between. LeaseBindingUnlessVisibility folds both into ONE transaction
// (bindings.go), so under the same single-connection pool no other
// transaction -- including a concurrent plain LeaseBinding call racing for
// the same target -- can be interleaved between the read and the write.
//
// This stress-races many guarded published-local leases against many plain
// private-local leases for the SAME target and checks the invariant the
// atomicity guarantees: every guarded lease that succeeded must have
// genuinely observed no live private-local/tether-hosted binding at its
// own commit point -- checkable after the fact via ListBindingsForTarget's
// full generation history, since a violation would mean a published-local
// binding's own immediately-preceding generation is a private-local
// binding that was ALREADY current (non-revoked, non-expired, ttl=0 here)
// at every later point in the history -- something the guard must never
// produce.
func TestBindings_LeaseUnlessVisibility_ConcurrentRaceNeverInterleaves(t *testing.T) {
	svc := newServiceForConcurrency(t)
	ctx := context.Background()
	blocked := []registry.PublicationVisibility{registry.VisibilityPrivateLocal, registry.VisibilityTetherHosted}

	const n = 12
	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Add(n * 2)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			<-start
			_, _ = svc.LeaseBinding(ctx, testTargetURN, "real-session", "local", "real-session", nil, registry.VisibilityPrivateLocal, 0)
		}(i)
		go func(i int) {
			defer wg.Done()
			<-start
			_, _ = svc.LeaseBindingUnlessVisibility(ctx, testTargetURN, "bridge-1", "host", "attempt",
				[]string{"pull-only"}, registry.VisibilityPublishedLocal, 0, blocked...)
		}(i)
	}
	close(start)
	wg.Wait()

	all, err := svc.ListBindingsForTarget(ctx, testTargetURN)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	byGen := make(map[int64]registry.RuntimeBinding, len(all))
	var maxGen int64
	for _, b := range all {
		byGen[b.Generation] = b
		if b.Generation > maxGen {
			maxGen = b.Generation
		}
	}
	// Every private-local binding in this test is never revoked and has no
	// ttl, so it is "live" for the rest of the run once committed. The
	// atomicity invariant: no published-local binding's immediately
	// preceding generation is a still-live private-local/tether-hosted
	// binding -- if the old two-call race existed, a guarded lease could
	// commit generation G+1 as published-local right after an already-
	// committed private-local generation G, which this loop would catch.
	for gen := int64(2); gen <= maxGen; gen++ {
		cur, ok := byGen[gen]
		if !ok || cur.Visibility != registry.VisibilityPublishedLocal {
			continue
		}
		prev, ok := byGen[gen-1]
		if !ok {
			continue
		}
		if prev.Visibility == registry.VisibilityPrivateLocal || prev.Visibility == registry.VisibilityTetherHosted {
			t.Fatalf("generation %d (published-local) directly supersedes generation %d (%s) -- the guarded lease minted over a Tether-managed binding, the exact race this primitive exists to close", gen, gen-1, prev.Visibility)
		}
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
