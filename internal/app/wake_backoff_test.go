package app

import (
	"context"
	"testing"
	"time"

	"github.com/hollis-labs/libs/harness/core/adapters/agentsessions"
	messaging "github.com/hollis-labs/libs/mesh/core/messaging"
	"github.com/hollis-labs/libs/mesh/core/messaging/delivery"

	"github.com/hollis-labs/tether/internal/registry"
)

// CW-20261030-0176: a wake retry delay doubles from its base, is capped, is
// jittered by +/-20%, and from wakeMaxAttempts on jumps to the message-age bound.
func TestWakeRetryDelayExponentialCappedAndExhausted(t *testing.T) {
	prevAge := wakeMaxMessageAge
	wakeMaxMessageAge = 6 * time.Hour
	t.Cleanup(func() { wakeMaxMessageAge = prevAge })
	want := []time.Duration{5, 10, 20, 40, 80, 160, 300, 300, 300, 300, 300}
	for i, w := range want {
		if got := wakeRetryDelay(i+1, 5*time.Second); got != min(w*time.Second, 300*time.Second) {
			t.Fatalf("attempt %d: delay %s, want %s", i+1, got, w*time.Second)
		}
	}
	if got := wakeRetryDelay(wakeMaxAttempts, 5*time.Second); got != 6*time.Hour {
		t.Fatalf("exhausted delay %s, want the message-age bound", got)
	}
	if got := wakeRetryDelay(3, 0); got != 0 {
		t.Fatalf("zero base must stay zero (tests make a nack immediately ready), got %s", got)
	}
}

func TestWakeRetryDelayJitterStaysWithinTwentyPercent(t *testing.T) {
	prev := wakeRetryJitter
	t.Cleanup(func() { wakeRetryJitter = prev })
	wakeRetryJitter = func() float64 { return 0.8 }
	if got := wakeRetryDelay(3, 5*time.Second); got != 16*time.Second {
		t.Fatalf("low jitter: %s", got)
	}
	wakeRetryJitter = func() float64 { return 1.2 }
	if got := wakeRetryDelay(3, 5*time.Second); got != 24*time.Second {
		t.Fatalf("high jitter: %s", got)
	}
}

// CW-20261030-0176: several mails for one busy session cost one wake attempt
// per sweep pass, not one per mail; the others stay ready, with no attempt row.
func TestRunWakeSweep_BusySessionGetsOneAttemptPerPass(t *testing.T) {
	st, reg := newWakeHarness(t)
	ctx := context.Background()
	rt := newFakeRuntime()
	rt.setAlive("s1", true, agentsessions.LiveStateProcessing)
	to := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "worker"}
	if _, err := reg.LeaseBinding(ctx, registry.LogicalAgentBindingTarget("worker"), "s1", "local", "s1", nil, "", 0); err != nil {
		t.Fatalf("lease: %v", err)
	}
	var ids []string
	for i := 0; i < 3; i++ {
		env := sendMessage(t, st, to)
		id, _, _ := st.DeliveryIDForMessage(ctx, env.ID)
		ids = append(ids, id)
	}
	if _, err := runWakeSweep(ctx, st, reg, rt.seam(), nil); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	attempts := 0
	for _, id := range ids {
		rd, err := st.DeliveryStore().GetDelivery(ctx, delivery.DeliveryID(id))
		if err != nil {
			t.Fatal(err)
		}
		attempts += rd.AttemptCount
	}
	if attempts != 1 {
		t.Fatalf("%d wake attempts for 3 mails to one busy session, want 1", attempts)
	}
}
