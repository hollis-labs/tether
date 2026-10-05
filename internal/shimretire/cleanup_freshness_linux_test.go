//go:build linux

package shimretire

import (
	"context"
	"errors"
	"slices"
	"testing"
)

func TestRetirementPriorObligationsBeforeLease(t *testing.T) {
	for _, kind := range []string{"authority", "acquire", "cancel", "valid", "foreign", "invalid_phase", "invalid_proof"} {
		t.Run(kind, func(t *testing.T) {
			r, p, store, lease, events := retireFixture()
			lease.cleanupErr = errors.New("uncertain cleanup")
			first := RetireAbsent(context.Background(), r, p)
			if first.Outcome != RetiredCleanupPending || !slices.Contains(first.Obligations, "cleanup_pending") || ValidateReceipt(store.receipt) != nil {
				t.Fatalf("invalid prior pending fixture: %+v", first)
			}
			prior := store.receipt.Clone()
			*events = nil
			lease.cleanupErr = nil
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch kind {
			case "authority":
				p.Validate = func(context.Context, Request) error { return errors.New("revoked") }
			case "acquire":
				p.Acquire = func(context.Context, Placement) (Lease, error) { return nil, errors.New("custody unavailable") }
			case "cancel":
				cancel()
			case "foreign":
				store.receipt.RequestDigest = "foreign"
			case "invalid_phase":
				store.receipt.Phase = "unknown"
			case "invalid_proof":
				store.receipt.Proof.Unit.Invocation = "foreign"
			}
			out := RetireAbsent(ctx, r, p)
			t.Logf("kind=%s prior=%v returned=%v durable=%v effects=%v", kind, prior.Obligations, out.Obligations, store.receipt.Obligations, *events)
			admitted := kind != "foreign" && kind != "invalid_phase" && kind != "invalid_proof"
			if slices.Contains(out.Obligations, "cleanup_pending") != admitted {
				t.Errorf("trusted earlier recovery obligation omitted from result: %+v", out)
			}
			if kind != "valid" && len(*events) != 0 {
				t.Errorf("refusal performed effects: %v", *events)
			}
		})
	}
}
