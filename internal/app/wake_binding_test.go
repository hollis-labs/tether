package app

import (
	"context"
	"testing"

	"github.com/hollis-labs/agentkit/agentsessions"
	messaging "github.com/hollis-labs/go-messaging"
	"github.com/hollis-labs/go-messaging/delivery"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/registry"
)

// An explicit notify session and a legacy message cannot bypass the
// pull-only fence, even when the bridge names a live hosted session.
func TestAttemptWake_PullOnlyOverrideNeverClaimsOrSubmits(t *testing.T) {
	for _, lapsed := range []bool{false, true} {
		for _, tracked := range []bool{false, true} {
			t.Run(testWakeBindingName(lapsed, tracked), func(t *testing.T) {
				st, reg := newWakeHarness(t)
				ctx := context.Background()
				rt := newFakeRuntime()
				rt.setAlive("s1", true, agentsessions.LiveStateIdle)
				to := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "worker"}
				binding, err := reg.LeaseBinding(ctx, registry.LogicalAgentBindingTarget(to.ID), "s1", "bridge", "attempt", []string{api.PullOnlyCapability}, registry.VisibilityPublishedLocal, 0)
				if err != nil {
					t.Fatal(err)
				}
				if lapsed {
					if err := reg.RevokeBinding(ctx, binding.ID); err != nil {
						t.Fatal(err)
					}
				}
				messageID := "legacy-untracked"
				if tracked {
					messageID = sendMessage(t, st, to).ID
				}
				outcome := attemptWake(ctx, st, reg, rt.seam(), messageID, to, "s1", "wake")
				if outcome.Reason != "pull-only" || outcome.Attempted || outcome.Delivered {
					t.Fatalf("wake = %+v; want pull-only without submission", outcome)
				}
				if rt.sendCallCount("s1") != 0 {
					t.Fatal("pull-only actor received a wake")
				}
				if tracked {
					id, _, _ := st.DeliveryIDForMessage(ctx, messageID)
					rd, err := st.DeliveryStore().GetDelivery(ctx, delivery.DeliveryID(id))
					if err != nil || rd.Status != delivery.DeliveryPending || rd.AttemptCount != 0 {
						t.Fatalf("delivery = %+v, err = %v; want untouched pending mail", rd, err)
					}
				}
			})
		}
	}
}

func testWakeBindingName(lapsed, tracked bool) string {
	name := "live"
	if lapsed {
		name = "revoked"
	}
	if tracked {
		return name + "/tracked"
	}
	return name + "/legacy"
}

// Mutate ownership at the runtime-health seam after Claim has recorded
// its generation. No wall-clock race or real provider process is needed.
func TestAttemptWake_BindingChangesDuringHealthReleaseClaim(t *testing.T) {
	for _, change := range []string{"same-session", "different-session", "pull-only", "revoked", "lookup-error"} {
		t.Run(change, func(t *testing.T) {
			st, reg := newWakeHarness(t)
			ctx := context.Background()
			rt := newFakeRuntime()
			rt.setAlive("s1", true, agentsessions.LiveStateIdle)
			rt.setAlive("s2", true, agentsessions.LiveStateIdle)
			to := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "worker"}
			target := registry.LogicalAgentBindingTarget(to.ID)
			original, err := reg.LeaseBinding(ctx, target, "s1", "local", "attempt-1", nil, registry.VisibilityTetherHosted, 0)
			if err != nil {
				t.Fatal(err)
			}
			env := sendMessage(t, st, to)
			seam := rt.seam()
			health := seam.health
			seam.health = func(id string) (api.RuntimeHealthResult, bool) {
				switch change {
				case "revoked":
					err = reg.RevokeBinding(ctx, original.ID)
				case "lookup-error":
					_, err = st.DB().ExecContext(ctx, "DROP TABLE runtime_bindings")
				default:
					sessionID, caps := "s1", []string(nil)
					if change == "different-session" {
						sessionID = "s2"
					}
					if change == "pull-only" {
						caps = []string{api.PullOnlyCapability}
					}
					_, err = reg.LeaseBinding(ctx, target, sessionID, "replacement", "attempt-2", caps, registry.VisibilityPrivateLocal, 0)
				}
				if err != nil {
					t.Fatal(err)
				}
				return health(id)
			}
			outcome := attemptWake(ctx, st, reg, seam, env.ID, to, "s1", "wake")
			want := "stale-generation"
			if change == "pull-only" {
				want = "pull-only"
			}
			if change == "lookup-error" {
				want = "binding-check-failed"
			}
			if outcome.Reason != want || outcome.Delivered {
				t.Fatalf("wake = %+v; want %s without submission", outcome, want)
			}
			if rt.sendCallCount("s1") != 0 || rt.sendCallCount("s2") != 0 {
				t.Fatal("wake submitted after ownership changed")
			}
			id, _, _ := st.DeliveryIDForMessage(ctx, env.ID)
			rd, err := st.DeliveryStore().GetDelivery(ctx, delivery.DeliveryID(id))
			if err != nil || rd.Status != delivery.DeliveryRetryScheduled {
				t.Fatalf("delivery = %+v, err = %v; want released for retry", rd, err)
			}
			receipts, err := st.DeliveryStore().Receipts(ctx, delivery.DeliveryID(id))
			if err != nil {
				t.Fatal(err)
			}
			for _, receipt := range receipts {
				if receipt.Stage == delivery.StageTurnSubmitted || receipt.Stage == delivery.StageConsumed {
					t.Fatalf("false submission/consumption receipt: %+v", receipt)
				}
			}
		})
	}
}

func TestAttemptWake_BindingLookupFailureNeverFallsBackToDirect(t *testing.T) {
	st, reg := newWakeHarness(t)
	ctx := context.Background()
	rt := newFakeRuntime()
	rt.setAlive("s1", true, agentsessions.LiveStateIdle)
	if _, err := st.DB().ExecContext(ctx, "DROP TABLE runtime_bindings"); err != nil {
		t.Fatal(err)
	}
	to := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "worker"}
	outcome := attemptWake(ctx, st, reg, rt.seam(), "legacy", to, "s1", "wake")
	if outcome.Reason != "binding-check-failed" || outcome.Delivered || rt.sendCallCount("s1") != 0 {
		t.Fatalf("wake = %+v; binding lookup failure must block direct submission", outcome)
	}
}

func TestAttemptWake_PinnedSessionRemainsIndependentOfActorBinding(t *testing.T) {
	st, reg := newWakeHarness(t)
	ctx := context.Background()
	rt := newFakeRuntime()
	rt.setAlive("s1", true, agentsessions.LiveStateIdle)
	if _, err := reg.LeaseBinding(ctx, registry.LogicalAgentBindingTarget("worker"), "s1", "bridge", "attempt", []string{api.PullOnlyCapability}, registry.VisibilityPublishedLocal, 0); err != nil {
		t.Fatal(err)
	}
	to := messaging.Address{Kind: messaging.KindSession, Authority: "test", ID: "s1"}
	env := sendMessage(t, st, to)
	outcome := attemptWake(ctx, st, reg, rt.seam(), env.ID, to, "s1", "wake")
	if !outcome.Delivered || rt.sendCallCount("s1") != 1 {
		t.Fatalf("pinned session wake = %+v; want submission to the exact session", outcome)
	}
}
