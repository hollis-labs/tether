package app

// CW-20261001-0016: a wake whose turn was submitted waits for the recipient's
// consumption instead of lapsing into retry, and a message the recipient has
// already handled is settled rather than woken again.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hollis-labs/substrate/harness/adapters/agentsessions"
	messaging "github.com/hollis-labs/substrate/mesh/messaging"
	"github.com/hollis-labs/substrate/mesh/messaging/delivery"

	"github.com/hollis-labs/tether/internal/store"
)

type fixedClock time.Time

func (c fixedClock) Now() time.Time { return time.Time(c) }

func deliveryIDOf(t *testing.T, st *store.Store, messageID string) delivery.DeliveryID {
	t.Helper()
	id, ok, err := st.DeliveryIDForMessage(context.Background(), messageID)
	if err != nil || !ok {
		t.Fatalf("delivery id for %s: ok=%v err=%v", messageID, ok, err)
	}
	return delivery.DeliveryID(id)
}

// deliveryAt reads a delivery as the delivery core sees it at time at,
// through a second store over the same DB with its clock moved there, so
// lease expiry runs as it would then. The read writes whatever expiry it
// finds, exactly as a real read at that time would.
func deliveryAt(t *testing.T, st *store.Store, id delivery.DeliveryID, at time.Time) delivery.RecipientDelivery {
	t.Helper()
	ds := delivery.NewSQLiteStore(st.DB(), delivery.WithSQLiteClock(fixedClock(at)))
	rd, err := ds.GetDelivery(context.Background(), id)
	if err != nil {
		t.Fatalf("get delivery at %s: %v", at, err)
	}
	return rd
}

func currentDelivery(t *testing.T, st *store.Store, id delivery.DeliveryID) delivery.RecipientDelivery {
	t.Helper()
	rd, err := st.DeliveryStore().GetDelivery(context.Background(), id)
	if err != nil {
		t.Fatalf("get delivery: %v", err)
	}
	return rd
}

func attemptsOf(t *testing.T, st *store.Store, id delivery.DeliveryID) []delivery.Attempt {
	t.Helper()
	attempts, err := st.DeliveryStore().Attempts(context.Background(), id)
	if err != nil {
		t.Fatalf("attempts: %v", err)
	}
	return attempts
}

// makeDue moves a retry_scheduled delivery's backoff into the past, as the
// existing sweep tests do (the shared store has no clock seam).
func makeDue(t *testing.T, st *store.Store, id delivery.DeliveryID) {
	t.Helper()
	if _, err := st.DB().ExecContext(context.Background(), "UPDATE messaging_deliveries SET next_attempt_at=? WHERE id=?",
		time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano), string(id)); err != nil {
		t.Fatalf("advance next_attempt_at: %v", err)
	}
}

// The smoke case: notify woke the session and its turn was submitted, and
// the agent pulled the message from its own inbox well after the 30s claim
// lease. The delivery must not lapse into retry_scheduled in between, and
// the recipient's pull settles it through the wake's own attempt.
func TestAttemptWake_SubmittedWakeAwaitsConsumptionThenSettles(t *testing.T) {
	st, reg := newWakeHarness(t)
	ctx := context.Background()
	rt := newFakeRuntime()
	rt.setAlive("s1", true, agentsessions.LiveStateIdle)

	to := messaging.Address{Kind: messaging.KindSession, Authority: "test", ID: "s1"}
	env := sendMessage(t, st, to)
	if outcome := attemptWake(ctx, st, reg, rt.seam(), env.ID, to, "s1", "wake up"); !outcome.Delivered {
		t.Fatalf("wake = %+v; want Delivered", outcome)
	}
	id := deliveryIDOf(t, st, env.ID)

	// 31s on, a 30s claim lease would have lapsed into retry_scheduled.
	rd := deliveryAt(t, st, id, time.Now().Add(wakeClaimLeaseDuration+time.Second))
	if rd.Status != delivery.DeliveryLeased {
		t.Fatalf("status 31s after a submitted wake = %q; want leased, awaiting consumption", rd.Status)
	}
	attempts := attemptsOf(t, st, id)
	if len(attempts) != 1 || attempts[0].Stage != delivery.StageTurnSubmitted {
		t.Fatalf("attempts = %+v; want one attempt at turn_submitted (never consumption)", attempts)
	}

	// The recipient's own inbox pull, as GET /messages/inbox handles it for
	// a verified recipient session: pull, then consume what was pulled.
	pulled, err := st.MessagingStore().Inbox(ctx, to, messaging.Filter{})
	if err != nil || len(pulled) != 1 {
		t.Fatalf("inbox = %v, %v; want the one message", pulled, err)
	}
	if err := st.MessagingStore().Consume(ctx, pulled[0].ID, to); err != nil {
		t.Fatalf("consume: %v", err)
	}

	if rd := currentDelivery(t, st, id); rd.Status != delivery.DeliveryDelivered {
		t.Fatalf("status after the recipient's pull = %q; want delivered", rd.Status)
	}
	attempts = attemptsOf(t, st, id)
	if len(attempts) != 1 || attempts[0].Stage != delivery.StageConsumed {
		t.Fatalf("attempts = %+v; want the wake's own attempt closed at consumed", attempts)
	}
	if n := rt.sendCallCount("s1"); n != 1 {
		t.Fatalf("sendTurn calls = %d; want 1", n)
	}
}

// A wake nobody consumes within the window lapses into one retry, so the
// recipient is reminded rather than the message silently settled.
func TestAttemptWake_UnconsumedWakeLapsesIntoRetryAfterWindow(t *testing.T) {
	st, reg := newWakeHarness(t)
	ctx := context.Background()
	rt := newFakeRuntime()
	rt.setAlive("s1", true, agentsessions.LiveStateIdle)

	to := messaging.Address{Kind: messaging.KindSession, Authority: "test", ID: "s1"}
	env := sendMessage(t, st, to)
	if outcome := attemptWake(ctx, st, reg, rt.seam(), env.ID, to, "s1", "wake up"); !outcome.Delivered {
		t.Fatalf("wake = %+v; want Delivered", outcome)
	}
	id := deliveryIDOf(t, st, env.ID)

	rd := deliveryAt(t, st, id, time.Now().Add(wakeConsumeWindow+time.Minute))
	if rd.Status != delivery.DeliveryRetryScheduled {
		t.Fatalf("status after the window = %q; want retry_scheduled", rd.Status)
	}
	if a := attemptsOf(t, st, id); len(a) != 1 || a[0].Stage != delivery.StageFailed || a[0].Error != "lease expired" {
		t.Fatalf("attempts = %+v; want the lapsed wake attempt failed with lease expired", a)
	}
}

// A message the recipient consumed or marked read while its delivery sat in a
// retry backoff is settled by the next attempt, not woken again. Consume
// cannot settle it during the backoff itself: the delivery core refuses a
// claim before next_attempt_at.
func TestAttemptWake_HandledDuringBackoffSettlesWithoutWaking(t *testing.T) {
	for _, tc := range []struct {
		name   string
		handle func(ctx context.Context, ms store.InboxStore, id string, to messaging.Address) error
	}{
		{"consumed", func(ctx context.Context, ms store.InboxStore, id string, to messaging.Address) error {
			return ms.Consume(ctx, id, to)
		}},
		{"read", func(ctx context.Context, ms store.InboxStore, id string, to messaging.Address) error {
			return ms.MarkRead(ctx, id, to)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, reg := newWakeHarness(t)
			ctx := context.Background()
			rt := newFakeRuntime()
			rt.setAlive("s1", true, agentsessions.LiveStateProcessing)

			to := messaging.Address{Kind: messaging.KindSession, Authority: "test", ID: "s1"}
			env := sendMessage(t, st, to)
			if outcome := attemptWake(ctx, st, reg, rt.seam(), env.ID, to, "s1", "wake up"); outcome.Reason != "busy" {
				t.Fatalf("wake = %+v; want busy", outcome)
			}
			id := deliveryIDOf(t, st, env.ID)

			if err := tc.handle(ctx, st.MessagingStore(), env.ID, to); err != nil {
				t.Fatalf("handle: %v", err)
			}
			if rd := currentDelivery(t, st, id); rd.Status != delivery.DeliveryRetryScheduled {
				t.Fatalf("status during the backoff = %q; want retry_scheduled", rd.Status)
			}

			makeDue(t, st, id)
			rt.setAlive("s1", true, agentsessions.LiveStateIdle)
			outcome := attemptWake(ctx, st, reg, rt.seam(), env.ID, to, "s1", "wake up")
			if outcome.Reason != "already-handled" || outcome.Attempted || outcome.Delivered {
				t.Fatalf("retry = %+v; want already-handled without a wake", outcome)
			}
			if n := rt.sendCallCount("s1"); n != 0 {
				t.Fatalf("sendTurn calls = %d; want 0", n)
			}
			if rd := currentDelivery(t, st, id); rd.Status != delivery.DeliveryDelivered {
				t.Fatalf("status = %q; want delivered", rd.Status)
			}
		})
	}
}

// A listing that is not the recipient's own -- an operator running
// `tether messages inbox` for someone else -- stamps delivered_at but settles
// nothing: the next attempt still wakes the recipient.
func TestAttemptWake_OperatorListingDoesNotSettle(t *testing.T) {
	st, reg := newWakeHarness(t)
	ctx := context.Background()
	rt := newFakeRuntime()
	rt.setAlive("s1", true, agentsessions.LiveStateProcessing)

	to := messaging.Address{Kind: messaging.KindSession, Authority: "test", ID: "s1"}
	env := sendMessage(t, st, to)
	if outcome := attemptWake(ctx, st, reg, rt.seam(), env.ID, to, "s1", "wake up"); outcome.Reason != "busy" {
		t.Fatalf("wake = %+v; want busy", outcome)
	}
	id := deliveryIDOf(t, st, env.ID)

	if listed, err := st.MessagingStore().Inbox(ctx, to, messaging.Filter{}); err != nil || len(listed) != 1 {
		t.Fatalf("operator listing = %v, %v; want the one message", listed, err)
	}
	makeDue(t, st, id)
	rt.setAlive("s1", true, agentsessions.LiveStateIdle)
	if outcome := attemptWake(ctx, st, reg, rt.seam(), env.ID, to, "s1", "wake up"); !outcome.Delivered {
		t.Fatalf("retry after an operator listing = %+v; want a real wake", outcome)
	}
	if n := rt.sendCallCount("s1"); n != 1 {
		t.Fatalf("sendTurn calls = %d; want 1", n)
	}
	if rd := currentDelivery(t, st, id); rd.Status != delivery.DeliveryLeased {
		t.Fatalf("status = %q; want leased, awaiting the recipient's consumption", rd.Status)
	}
}

// A wake whose turn was not accepted -- busy, offline, or a submit failure --
// still releases the delivery for retry, as before; nothing holds its lease.
func TestAttemptWake_NotAcceptedWakeStillRetries(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setup  func(rt *fakeRuntime)
		reason string
	}{
		{"busy", func(rt *fakeRuntime) { rt.setAlive("s1", true, agentsessions.LiveStateProcessing) }, "busy"},
		{"offline", func(rt *fakeRuntime) { rt.setAlive("s1", false, agentsessions.LiveStateStopped) }, "offline-race"},
		{"submit failed", func(rt *fakeRuntime) {
			rt.setAlive("s1", true, agentsessions.LiveStateIdle)
			rt.setSendErr("s1", errors.New("steering unsupported"))
		}, "turn-submit-failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, reg := newWakeHarness(t)
			ctx := context.Background()
			rt := newFakeRuntime()
			tc.setup(rt)

			to := messaging.Address{Kind: messaging.KindSession, Authority: "test", ID: "s1"}
			env := sendMessage(t, st, to)
			if outcome := attemptWake(ctx, st, reg, rt.seam(), env.ID, to, "s1", "wake up"); outcome.Reason != tc.reason || outcome.Delivered {
				t.Fatalf("wake = %+v; want reason %q, not delivered", outcome, tc.reason)
			}
			rd := currentDelivery(t, st, deliveryIDOf(t, st, env.ID))
			if rd.Status != delivery.DeliveryRetryScheduled || !rd.NextAttemptAt.After(time.Now()) {
				t.Fatalf("delivery = %s next=%s; want retry_scheduled with a future backoff", rd.Status, rd.NextAttemptAt)
			}
		})
	}
}
