package registry_test

// register_idempotent_test.go — coverage for T02's RegisterIdempotent
// (register_idempotent.go). Matrix:
//   - First call with a fresh (substrate, external_id) key mints and
//     inserts; created=true.
//   - Second call with the SAME key returns the SAME URN; created=false;
//     no duplicate row (two same-profile boots stay distinct is the
//     complementary case: two DIFFERENT keys must mint two DIFFERENT URNs,
//     covered below).
//   - Two different external keys with an identical DisplayName produce
//     two distinct URNs (same-profile boots stay distinct -- T02 acceptance
//     criterion 1).
//   - Concurrent RegisterIdempotent calls racing on the SAME key: exactly
//     one row is ever created; every caller observes the same URN.
//   - Validation: caller-supplied URN, empty display_name, empty
//     substrate/external_id, unsupported kind (group) all rejected.

import (
	"context"
	"sync"
	"testing"

	"github.com/hollis-labs/tether/internal/registry"
)

func TestRegisterIdempotent_FirstCallCreates(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()

	out, created, err := svc.RegisterIdempotent(ctx, registry.KindAgent, registry.Profile{DisplayName: "Nightly Planner"}, "tether", "logical_agent:planner-1")
	if err != nil {
		t.Fatalf("RegisterIdempotent: %v", err)
	}
	if !created {
		t.Fatalf("expected created=true on first call")
	}
	if out.URN == "" {
		t.Fatalf("expected a minted URN")
	}
	ext, ok := out.ExternalIDFor("tether")
	if !ok || ext.ExternalID != "logical_agent:planner-1" {
		t.Fatalf("expected external id attached, got %+v", out.ExternalIDs)
	}
}

func TestRegisterIdempotent_SecondCallSameKeyReturnsSameURN(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()

	first, created1, err := svc.RegisterIdempotent(ctx, registry.KindAgent, registry.Profile{DisplayName: "Nightly Planner"}, "tether", "logical_agent:planner-1")
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if !created1 {
		t.Fatalf("expected created=true on first call")
	}

	second, created2, err := svc.RegisterIdempotent(ctx, registry.KindAgent, registry.Profile{DisplayName: "Nightly Planner"}, "tether", "logical_agent:planner-1")
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if created2 {
		t.Fatalf("expected created=false on repeat call")
	}
	if second.URN != first.URN {
		t.Fatalf("expected the same URN on repeat call, got %q vs %q", second.URN, first.URN)
	}
}

func TestRegisterIdempotent_TwoSameProfileBootsStayDistinct(t *testing.T) {
	// T02 acceptance criterion 1: two same-profile boots stay distinct.
	// A one-off boot with no durable external key never collides with a
	// prior durable actor merely by sharing a display name/profile label.
	svc := newService(t)
	ctx := context.Background()

	a, _, err := svc.RegisterIdempotent(ctx, registry.KindAgent, registry.Profile{DisplayName: "Engineer"}, "tether", "logical_agent:engineer-instance-a")
	if err != nil {
		t.Fatalf("register a: %v", err)
	}
	b, _, err := svc.RegisterIdempotent(ctx, registry.KindAgent, registry.Profile{DisplayName: "Engineer"}, "tether", "logical_agent:engineer-instance-b")
	if err != nil {
		t.Fatalf("register b: %v", err)
	}
	if a.URN == b.URN {
		t.Fatalf("two distinct external keys must never collapse onto one URN, got %q for both", a.URN)
	}

	// Both remain independently resolvable afterward -- explicit provenance,
	// no silent merge.
	got, err := svc.Lookup(ctx, a.URN)
	if err != nil || got.URN != a.URN {
		t.Fatalf("instance a must remain resolvable: %v / %+v", err, got)
	}
	got, err = svc.Lookup(ctx, b.URN)
	if err != nil || got.URN != b.URN {
		t.Fatalf("instance b must remain resolvable: %v / %+v", err, got)
	}
}

func TestRegisterIdempotent_ConcurrentSameKeyMintsExactlyOnce(t *testing.T) {
	svc := newServiceForConcurrency(t)
	ctx := context.Background()

	const n = 8
	urns := make([]string, n)
	createdFlags := make([]bool, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			out, created, err := svc.RegisterIdempotent(ctx, registry.KindAgent, registry.Profile{DisplayName: "Racer"}, "tether", "logical_agent:racer")
			urns[i] = out.URN
			createdFlags[i] = created
			errs[i] = err
		}(i)
	}
	wg.Wait()

	createdCount := 0
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("caller %d: unexpected error: %v", i, errs[i])
		}
		if urns[i] == "" {
			t.Fatalf("caller %d: empty URN", i)
		}
		if urns[i] != urns[0] {
			t.Fatalf("caller %d observed a different URN than caller 0: %q vs %q -- the same external key must never mint two rows", i, urns[i], urns[0])
		}
		if createdFlags[i] {
			createdCount++
		}
	}
	if createdCount != 1 {
		t.Fatalf("expected exactly one caller to observe created=true, got %d", createdCount)
	}
}

func TestRegisterIdempotent_RepeatCallCannotTakeOverOrMutateExistingRow(t *testing.T) {
	// T02 acceptance criterion 3: idempotent publication must not let a
	// second caller take over an existing mapping. RegisterIdempotent
	// never mutates an existing row -- a repeat call with a DIFFERENT
	// display_name/role for the SAME external key returns the ORIGINAL
	// profile unchanged. A caller that wants to change fields on a row it
	// legitimately owns uses UpdateSelf (D5), a distinct, explicit
	// operation -- not a side effect of re-registering.
	svc := newService(t)
	ctx := context.Background()

	original, created, err := svc.RegisterIdempotent(ctx, registry.KindAgent, registry.Profile{
		DisplayName: "Original Name",
		Role:        "original-role",
	}, "tether", "logical_agent:takeover-target")
	if err != nil || !created {
		t.Fatalf("initial register: created=%v err=%v", created, err)
	}

	attempt, created2, err := svc.RegisterIdempotent(ctx, registry.KindAgent, registry.Profile{
		DisplayName: "Hijacked Name",
		Role:        "attacker-role",
	}, "tether", "logical_agent:takeover-target")
	if err != nil {
		t.Fatalf("takeover attempt: %v", err)
	}
	if created2 {
		t.Fatalf("expected created=false -- a repeat call must never mint a second identity for an already-claimed key")
	}
	if attempt.URN != original.URN {
		t.Fatalf("expected the same URN, got %q vs original %q", attempt.URN, original.URN)
	}
	if attempt.DisplayName != "Original Name" || attempt.Role != "original-role" {
		t.Fatalf("takeover attempt must not change the existing row's fields, got display_name=%q role=%q", attempt.DisplayName, attempt.Role)
	}

	// Confirm the stored row itself was never touched.
	reloaded, err := svc.Lookup(ctx, original.URN)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if reloaded.DisplayName != "Original Name" || reloaded.Role != "original-role" {
		t.Fatalf("stored row was mutated by a repeat RegisterIdempotent call: %+v", reloaded)
	}
}

func TestRegisterIdempotent_Validation(t *testing.T) {
	svc := newService(t)
	ctx := context.Background()

	if _, _, err := svc.RegisterIdempotent(ctx, registry.KindAgent, registry.Profile{URN: "msg://agent/agent-mux/agt_alreadyset", DisplayName: "X"}, "tether", "k"); err == nil {
		t.Fatalf("expected error for caller-supplied URN")
	}
	if _, _, err := svc.RegisterIdempotent(ctx, registry.KindAgent, registry.Profile{}, "tether", "k"); err == nil {
		t.Fatalf("expected error for empty display_name")
	}
	if _, _, err := svc.RegisterIdempotent(ctx, registry.KindAgent, registry.Profile{DisplayName: "X"}, "", "k"); err == nil {
		t.Fatalf("expected error for empty substrate")
	}
	if _, _, err := svc.RegisterIdempotent(ctx, registry.KindAgent, registry.Profile{DisplayName: "X"}, "tether", ""); err == nil {
		t.Fatalf("expected error for empty external_id")
	}
	if _, _, err := svc.RegisterIdempotent(ctx, registry.KindGroup, registry.Profile{DisplayName: "X"}, "tether", "k"); err == nil {
		t.Fatalf("expected error for unsupported kind (group)")
	}
}
