package store_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	messaging "github.com/hollis-labs/libs/mesh/core/messaging"
	"github.com/hollis-labs/libs/mesh/core/messaging/delivery"

	"github.com/hollis-labs/tether/internal/store"
)

// CW-20261021-0029: marking a message read settles its delivery, so the trace
// shows it consumed and nothing wakes it afterwards.

func markReadFixture(t *testing.T) (*store.Store, messaging.Envelope, delivery.DeliveryID) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "markread.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	msg, err := db.MessagingStore().Send(context.Background(), messaging.Envelope{From: addr("alice"), To: addr("bob"), Kind: messaging.MsgKindNotice})
	if err != nil {
		t.Fatal(err)
	}
	id, ok, err := db.DeliveryIDForMessage(context.Background(), msg.ID)
	if err != nil || !ok {
		t.Fatalf("delivery id: ok=%v err=%v", ok, err)
	}
	return db, msg, delivery.DeliveryID(id)
}

func deliveryState(t *testing.T, db *store.Store, id delivery.DeliveryID) (delivery.DeliveryStatus, map[delivery.ReceiptStage]int) {
	t.Helper()
	del, err := db.DeliveryStore().GetDelivery(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	receipts, err := db.DeliveryStore().Receipts(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	stages := map[delivery.ReceiptStage]int{}
	for _, r := range receipts {
		stages[r.Stage]++
	}
	return del.Status, stages
}

func TestMarkReadSettlesTheDeliveryAsConsumed(t *testing.T) {
	db, msg, id := markReadFixture(t)
	ctx := context.Background()
	if err := db.MessagingStore().MarkRead(ctx, msg.ID, addr("bob")); err != nil {
		t.Fatal(err)
	}
	status, stages := deliveryState(t, db, id)
	if status != delivery.DeliveryDelivered {
		t.Fatalf("delivery status %q after mark-read, want delivered", status)
	}
	for _, want := range []delivery.ReceiptStage{delivery.StageHostAccepted, delivery.StageTurnSubmitted, delivery.StageConsumed} {
		if stages[want] != 1 {
			t.Fatalf("receipt %q recorded %d times: %v", want, stages[want], stages)
		}
	}
	handled, err := db.MessageHandledByRecipient(ctx, msg.ID)
	if err != nil || !handled {
		t.Fatalf("read message must be handled: %v %v", handled, err)
	}
	// Attention state is not consumption.
	var consumed *string
	if err := db.DB().QueryRow(`SELECT consumed_at FROM messages WHERE id=?`, msg.ID).Scan(&consumed); err != nil || consumed != nil {
		t.Fatalf("mark-read must not stamp consumed_at: %v %v", consumed, err)
	}
	// Replay: no error, no duplicate receipts.
	if err := db.MessagingStore().MarkRead(ctx, msg.ID, addr("bob")); err != nil {
		t.Fatal(err)
	}
	_, again := deliveryState(t, db, id)
	for stage, n := range stages {
		if again[stage] != n {
			t.Fatalf("replay changed receipts: before %v after %v", stages, again)
		}
	}
	// A later Consume after the read stays a no-op for the delivery.
	if err := db.MessagingStore().Consume(ctx, msg.ID, addr("bob")); err != nil {
		t.Fatal(err)
	}
	if _, afterConsume := deliveryState(t, db, id); afterConsume[delivery.StageConsumed] != 1 {
		t.Fatalf("consume after read duplicated the consumed receipt: %v", afterConsume)
	}
}

// A wake that already submitted the turn holds the lease open for the recipient;
// marking the message read closes that same lease instead of leaving it to lapse
// and be retried.
func TestMarkReadClosesTheWakeLeaseOnTheMessage(t *testing.T) {
	db, msg, id := markReadFixture(t)
	ctx := context.Background()
	claim, err := db.DeliveryStore().Claim(ctx, delivery.ClaimRequest{DeliveryID: id, Holder: "sess", LeaseDuration: time.Minute, Nowait: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetPendingReceiptLease(ctx, msg.ID, claim.Attempt.ID, claim.Attempt.LeaseToken); err != nil {
		t.Fatal(err)
	}
	lease := delivery.LeaseRef{DeliveryID: id, AttemptID: claim.Attempt.ID, LeaseToken: claim.Attempt.LeaseToken}
	for _, stage := range []delivery.ReceiptStage{delivery.StageHostAccepted, delivery.StageTurnSubmitted} {
		if _, _, err := db.DeliveryStore().Ack(ctx, delivery.AckRequest{Lease: lease, Stage: stage}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.MessagingStore().MarkRead(ctx, msg.ID, addr("bob")); err != nil {
		t.Fatal(err)
	}
	if status, stages := deliveryState(t, db, id); status != delivery.DeliveryDelivered || stages[delivery.StageConsumed] != 1 {
		t.Fatalf("status %q receipts %v, want the wake's lease closed as consumed", status, stages)
	}
}

// A delivery released for retry (a busy or failed wake) has no lease; reading the
// message settles it once its backoff has elapsed, so the next sweep finds nothing
// to wake. While the backoff is still running the delivery cannot be claimed, so
// the read settles it at the next wake attempt instead (the wake path's handled
// check; see TestWakeOfAReadMessageSettlesInsteadOfSubmitting).
func TestMarkReadSettlesARetryScheduledDelivery(t *testing.T) {
	db, msg, id := markReadFixture(t)
	ctx := context.Background()
	claim, err := db.DeliveryStore().Claim(ctx, delivery.ClaimRequest{DeliveryID: id, Holder: "sess", LeaseDuration: time.Minute, Nowait: true})
	if err != nil {
		t.Fatal(err)
	}
	lease := delivery.LeaseRef{DeliveryID: id, AttemptID: claim.Attempt.ID, LeaseToken: claim.Attempt.LeaseToken}
	if _, _, err := db.DeliveryStore().Nack(ctx, delivery.NackRequest{Lease: lease, Retryable: true, Error: "session busy", NextAttemptAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := db.MessagingStore().MarkRead(ctx, msg.ID, addr("bob")); err != nil {
		t.Fatal(err)
	}
	if status, _ := deliveryState(t, db, id); status != delivery.DeliveryDelivered {
		t.Fatalf("status %q, want delivered", status)
	}
}

// Another claimant's lease is never taken over by a read.
func TestMarkReadDoesNotHijackAnIndependentlyClaimedLease(t *testing.T) {
	db, msg, id := markReadFixture(t)
	ctx := context.Background()
	_, bridge, err := db.ClaimMessageDelivery(ctx, msg.ID, addr("bob"), "bob", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.MessagingStore().MarkRead(ctx, msg.ID, addr("bob")); err != nil {
		t.Fatal(err)
	}
	del, err := db.DeliveryStore().GetDelivery(ctx, id)
	if err != nil || del.Status != delivery.DeliveryLeased || del.ActiveAttemptID != bridge.AttemptID {
		t.Fatalf("a read took over another claimant's lease: %+v %v", del, err)
	}
}

// read_at committed but the receipts missing (a crash between the two writes):
// the caller's retry completes them.
func TestMarkReadReplayHealsMissingReceipts(t *testing.T) {
	db, msg, id := markReadFixture(t)
	ctx := context.Background()
	if _, err := db.DB().Exec(`UPDATE messages SET read_at=? WHERE id=?`, time.Now().UTC().Format(time.RFC3339Nano), msg.ID); err != nil {
		t.Fatal(err)
	}
	if status, _ := deliveryState(t, db, id); status == delivery.DeliveryDelivered {
		t.Fatalf("precondition: the delivery must still be open")
	}
	if err := db.MessagingStore().MarkRead(ctx, msg.ID, addr("bob")); err != nil {
		t.Fatal(err)
	}
	if status, _ := deliveryState(t, db, id); status != delivery.DeliveryDelivered {
		t.Fatalf("status %q after the replay, want delivered", status)
	}
}

// Only the addressed recipient can settle a delivery by reading.
func TestMarkReadByAnotherPartyDoesNotSettle(t *testing.T) {
	db, msg, id := markReadFixture(t)
	if err := db.MessagingStore().MarkRead(context.Background(), msg.ID, addr("carol")); err == nil {
		t.Fatal("a non-recipient's mark-read must fail")
	}
	if status, _ := deliveryState(t, db, id); status == delivery.DeliveryDelivered {
		t.Fatalf("a non-recipient's read settled the delivery")
	}
}

// A message that predates delivery tracking has no delivery to settle.
func TestMarkReadWithoutADeliveryIsJustARead(t *testing.T) {
	db, msg, _ := markReadFixture(t)
	if _, err := db.DB().Exec(`UPDATE messages SET delivery_id=NULL WHERE id=?`, msg.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.MessagingStore().MarkRead(context.Background(), msg.ID, addr("bob")); err != nil {
		t.Fatal(err)
	}
}
