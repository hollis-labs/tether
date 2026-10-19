package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/hollis-labs/libs/mesh/core/messaging"
	"github.com/hollis-labs/libs/mesh/core/messaging/delivery"
	"github.com/hollis-labs/libs/mesh/core/messaging/deliverytest"
	"github.com/hollis-labs/tether/internal/store"
)

func TestMessageEventsAutomaticExpiryAndNonmonotonicAttemptTimes(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	env := sendEventMessage(t, st)
	id, _, _ := st.DeliveryIDForMessage(ctx, env.ID)
	clock := deliverytest.NewFakeClock(time.Now().UTC())
	ds := delivery.NewSQLiteStore(st.DB(), delivery.WithSQLiteClock(clock))
	first, err := ds.Claim(ctx, delivery.ClaimRequest{DeliveryID: delivery.DeliveryID(id), Holder: "one", LeaseDuration: time.Second, BindingGeneration: 7})
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(2 * time.Second)
	if _, err = ds.ListDeliveries(ctx, delivery.Filter{ReadyOnly: true}); err != nil {
		t.Fatal(err)
	}
	// A second acquisition can have an earlier timestamp; ordinals still follow
	// committed claims, not Attempts' timestamp order or today's attempt count.
	clock.Advance(time.Second)
	second, err := ds.Claim(ctx, delivery.ClaimRequest{DeliveryID: delivery.DeliveryID(id), Holder: "two", LeaseDuration: time.Minute, BindingGeneration: 9})
	if err != nil {
		t.Fatal(err)
	}
	lease := delivery.LeaseRef{DeliveryID: second.Delivery.ID, AttemptID: second.Attempt.ID, LeaseToken: second.Attempt.LeaseToken, BindingGeneration: 9}
	if _, _, err = ds.Nack(ctx, delivery.NackRequest{Lease: lease, Retryable: false, Error: "ERROR-CANARY"}); err != nil && !errors.Is(err, delivery.ErrDeadLettered) {
		t.Fatal(err)
	}
	if _, err = ds.Redrive(ctx, delivery.RedriveRequest{DeliveryID: second.Delivery.ID, AuthorizedBy: env.From.URN()}); err != nil {
		t.Fatal(err)
	}
	clock.Set(first.Attempt.AcquiredAt.Add(-time.Second))
	third, err := ds.Claim(ctx, delivery.ClaimRequest{DeliveryID: second.Delivery.ID, Holder: "three", LeaseDuration: time.Minute, BindingGeneration: 11})
	if err != nil {
		t.Fatal(err)
	}
	thirdLease := delivery.LeaseRef{DeliveryID: third.Delivery.ID, AttemptID: third.Attempt.ID, LeaseToken: third.Attempt.LeaseToken, BindingGeneration: 11}
	if _, _, err = ds.Nack(ctx, delivery.NackRequest{Lease: thirdLease, Retryable: false, Error: "ERROR-CANARY"}); err != nil && !errors.Is(err, delivery.ErrDeadLettered) {
		t.Fatal(err)
	}
	rows := drainMessages(t, st)
	failures := 0
	for _, ev := range rows {
		payload := messageEventPayload(t, ev)
		if ev.Kind != "message.failed" {
			continue
		}
		failures++
		d := payload["delivery"].(map[string]any)
		if d["attempt_id"] == string(first.Attempt.ID) {
			if d["attempt"] != float64(1) || d["binding_generation"] != float64(7) || payload["reason"] != "lease_expired" {
				t.Fatal(payload)
			}
		} else if d["attempt_id"] == string(second.Attempt.ID) {
			if d["attempt"] != float64(2) || d["binding_generation"] != float64(9) {
				t.Fatal(payload)
			}
		} else if d["attempt_id"] == string(third.Attempt.ID) {
			if d["attempt"] != float64(3) || d["binding_generation"] != float64(11) {
				t.Fatal(payload)
			}
		} else {
			t.Fatal("unknown attempt")
		}
	}
	if failures != 3 {
		t.Fatalf("got %d failed observations", failures)
	}
}

func TestMessageEventsAutomaticDeadlineFromLibraryLog(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	clock := deliverytest.NewFakeClock(time.Now().UTC())
	ds := delivery.NewSQLiteStore(st.DB(), delivery.WithSQLiteClock(clock))
	from := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "sender"}
	to := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "recipient"}
	result, err := ds.Enqueue(ctx, delivery.EnqueueRequest{From: from, Recipients: []delivery.RecipientTarget{{Address: to}}, Kind: messaging.MsgKindNotice, DeadlineAt: time.Now().UTC().Add(time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.DB().Exec(`INSERT INTO messages(id,kind,channel,from_urn,to_urn,created_at,delivery_id) VALUES (?,'notice','',?,?,?,?)`, result.Message.ID, from.URN(), to.URN(), result.Message.CreatedAt.Format(time.RFC3339Nano), result.Deliveries[0].ID); err != nil {
		t.Fatal(err)
	}
	clock.Advance(2 * time.Second)
	if _, err = ds.ListDeliveries(ctx, delivery.Filter{}); err != nil {
		t.Fatal(err)
	}
	// Pending deadlines are enforced by the library on a targeted claim, not
	// by its lease-expiry listing. This is the actual shipped state machine.
	if _, err = ds.Claim(ctx, delivery.ClaimRequest{DeliveryID: result.Deliveries[0].ID, Holder: "deadline", LeaseDuration: time.Minute}); err == nil {
		t.Fatal("expired delivery claimed")
	}
	rows := drainMessages(t, st)
	seen := false
	for _, ev := range rows {
		payload := messageEventPayload(t, ev)
		if ev.Kind == "message.dead_lettered" {
			seen = true
			if payload["reason"] != "deadline_exceeded" || payload["delivery"].(map[string]any)["attempt"] != float64(0) {
				t.Fatal(payload)
			}
		}
	}
	if !seen {
		t.Fatal("automatic deadline transition missing")
	}
}

func TestMessageEventsProjectionDoesNotDriveDeliveryExpiry(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	env := sendEventMessage(t, st)
	id, _, _ := st.DeliveryIDForMessage(ctx, env.ID)
	clock := deliverytest.NewFakeClock(time.Now().UTC().Add(-5 * time.Second))
	ds := delivery.NewSQLiteStore(st.DB(), delivery.WithSQLiteClock(clock))
	if _, err = ds.Claim(ctx, delivery.ClaimRequest{DeliveryID: delivery.DeliveryID(id), Holder: "holder", LeaseDuration: time.Second}); err != nil {
		t.Fatal(err)
	}
	drainMessages(t, st)
	var state string
	if err = st.DB().QueryRow(`SELECT status FROM messaging_deliveries WHERE id=?`, id).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != string(delivery.DeliveryLeased) {
		t.Fatalf("projection advanced library state: %s", state)
	}
	// The existing delivery reader remains in charge of actual expiry.
	if _, err = st.DeliveryStore().ListDeliveries(ctx, delivery.Filter{}); err != nil {
		t.Fatal(err)
	}
	rows := drainMessages(t, st)
	seen := false
	for _, ev := range rows {
		if ev.Kind == "message.failed" {
			seen = true
		}
	}
	if !seen {
		t.Fatal("library expiry was not projected")
	}
}
