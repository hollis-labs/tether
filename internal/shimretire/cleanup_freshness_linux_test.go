//go:build linux

package shimretire

import (
	"context"
	"errors"
	"os"
	"slices"
	"testing"
	"time"
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

func TestRetirementCleanupExpiryAfterFinalCallback(t *testing.T) {
	for _, kind := range []string{"expires", "valid"} {
		t.Run(kind, func(t *testing.T) {
			cleanup, artifact, store, name := cleanupFixture(t)
			now := cleanup.Now()
			calls := 0
			cleanup.Now = func() time.Time { return now }
			cleanup.Validate = func(context.Context, Request) error {
				calls++
				if calls == 2 && kind == "expires" {
					now = store.receipt.Proof.ValidUntil.Add(time.Nanosecond)
				}
				return nil
			}
			mutation, err := cleanup.Descriptor(context.Background(), store.receipt.OperationID, artifact)
			_, statErr := os.Stat(name)
			t.Logf("callbacks=%d now=%s proofvaliduntil=%s mutation=%s err=%v file_exists=%t", calls, now, store.receipt.Proof.ValidUntil, mutation, err, statErr == nil)
			if calls != 2 {
				t.Fatal("final callback not reached")
			}
			if kind == "expires" {
				if mutation != NoChange || err == nil || statErr != nil {
					t.Errorf("expired proof after final callback permits cleanup: mutation=%s err=%v stat=%v", mutation, err, statErr)
				}
			} else if mutation != Changed || err != nil || !os.IsNotExist(statErr) {
				t.Errorf("unchanged valid proof positive failed: %s %v %v", mutation, err, statErr)
			}
		})
	}
}

func TestRetirementDescriptorPriorObligationPositive(t *testing.T) {
	cleanup, artifact, store, name := cleanupFixture(t)
	store.receipt.Obligations = []string{"cleanup_pending"}
	mutation, err := cleanup.Descriptor(context.Background(), store.receipt.OperationID, artifact)
	_, statErr := os.Stat(name)
	if mutation != Changed || err != nil || !os.IsNotExist(statErr) || !slices.Contains(store.receipt.Obligations, "cleanup_pending") {
		t.Fatalf("original descriptor retry lost obligation or stopped valid cleanup: %s %v %v %v", mutation, err, statErr, store.receipt.Obligations)
	}
}

func TestRetirementRetentionExpiryAfterFinalCallback(t *testing.T) {
	for _, kind := range []string{"expires", "valid"} {
		t.Run(kind, func(t *testing.T) {
			cleanup, descriptor, audit, _ := cleanupFixture(t)
			f, err := cleanup.Root.OpenFile("host.log", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.Write([]byte("private")); err != nil {
				t.Fatal(err)
			}
			info, err := f.Stat()
			if err != nil {
				t.Fatal(err)
			}
			if err = f.Close(); err != nil {
				t.Fatal(err)
			}
			a := descriptor
			a.Category = HostLog
			a.RelativePath = "host.log"
			a.Identity = fileIdentity(info)
			a.Size = 7
			audit.receipt.Phase = StateReconciled
			audit.receipt.Inventory = []Artifact{descriptor, a}
			now := cleanup.Now()
			audit.receipt.RetiredAt = now.Add(-time.Hour)
			cleanup.Now = func() time.Time { return now }
			request, _, _, _, _ := retentionFixture()
			request.Policy.RootIDs = []string{a.RootID}
			candidate := SweepCandidate{RetirementOperation: audit.receipt.OperationID, Artifact: a, SortKey: "retired/session/host.log", Hold: NoRetentionHold, HoldRevision: "holds"}
			cursors := &memoryCursors{cursor: SweepCursor{Version: RetentionVersion, ID: request.OperationID, Request: request, RequestDigest: retentionDigest(request), Revision: "cursor", InventoryRevision: "inventory", Phase: CursorIntent, Pending: &candidate}}
			calls := 0
			cleanup.Validate = func(context.Context, Request) error {
				calls++
				if calls == 2 && kind == "expires" {
					now = audit.receipt.Proof.ValidUntil.Add(time.Nanosecond)
				}
				return nil
			}
			removal := RetentionRemoval{Cleanup: cleanup, Request: request, Cursors: cursors, Validate: func(context.Context, SweepRequest) error { return nil }, Observe: func(context.Context, SweepCandidate) (SweepCandidate, Snapshot, Observation, error) {
				return candidate, audit.receipt.Snapshot, audit.receipt.Proof, nil
			}}
			mutation, err := removal.Remove(context.Background(), candidate)
			b, readErr := cleanup.Root.ReadFile("host.log")
			t.Logf("callbacks=%d now=%s validuntil=%s mutation=%s err=%v readerr=%v cursorphase=%s", calls, now, audit.receipt.Proof.ValidUntil, mutation, err, readErr, cursors.cursor.Phase)
			if calls != 2 {
				t.Fatal("final callback not reached")
			}
			if kind == "expires" {
				if mutation != NoChange || err == nil || readErr != nil || string(b) != "private" {
					t.Errorf("expired retention proof permits actual removal: %s %v %v", mutation, err, readErr)
				}
			} else if mutation != Changed || err != nil || !os.IsNotExist(readErr) {
				t.Errorf("valid retention positive failed: %s %v %v", mutation, err, readErr)
			}
		})
	}
}

func TestRetirementFinalClockPrecedesPhysicalCustody(t *testing.T) {
	for _, kind := range []string{"replace", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			cleanup, a, _, name := cleanupFixture(t)
			now := cleanup.Now()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			clocks := 0
			cleanup.Now = func() time.Time {
				clocks++
				if clocks == 2 {
					if kind == "replace" {
						if err := os.Rename(name, name+".old"); err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(name, []byte("changed"), 0600); err != nil {
							t.Fatal(err)
						}
					} else {
						cancel()
					}
				}
				return now
			}
			mutation, err := cleanup.Descriptor(ctx, "retirement", a)
			want := "private"
			if kind == "replace" {
				want = "changed"
			}
			b, e := os.ReadFile(name)
			if mutation != NoChange || err == nil || e != nil || string(b) != want || clocks != 2 {
				t.Fatalf("clock bypassed physical/context custody: %s %v bytes=%q err=%v clocks=%d", mutation, err, b, e, clocks)
			}
		})
	}
}
