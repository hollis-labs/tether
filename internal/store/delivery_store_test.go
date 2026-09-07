package store_test

// delivery_store_test.go — T03 (messaging vNext, CW-20260904-0100).
// Acceptance evidence:
//
//  1. TestDeliveryStore_Contract — go-messaging's own deliverytest.RunStoreContract
//     against Tether's actual wiring (Store.DeliveryStore(), same *sql.DB
//     configuration store.Open uses: WAL, busy_timeout, MaxOpenConns=1).
//  2. TestDeliveryStore_SurvivesRestart — closes and reopens the *sql.DB
//     against the same file path; enqueued/claimed/acked state survives.
//  3. TestDeliveryStore_ConcurrentClaimsAcrossIndependentConnections — TWO
//     separate *sql.DB handles (not goroutines sharing one connection pool)
//     racing Claim on the same delivery, simulating the real daemon-process
//     vs. mux-mcp-subprocess topology (T01 contract §2.7) rather than only
//     an in-process Go race test.
//  4. TestDeliveryStore_CrashInjection_PartialEnqueueRollsBack /
//     TestDeliveryStore_CrashInjection_PartialClaimRollsBack — WithSQLiteMutationHook
//     crash injection proving no partial state survives a failure mid-transaction.
//  5. TestDeliveryBackedSend_* / TestDeliveryBackedConsume_* — the
//     deliveryBackedStore decorator's own integration behavior: Send creates
//     a real delivery obligation sharing the message's own ID; Consume
//     drives host_accepted/turn_submitted/consumed receipts, idempotently,
//     on every call that has a delivery_id (fixed for CW-20260907-0033 --
//     see TestDeliveryBackedConsume_RecoversReceiptsAfterCrashBeforeAck --
//     receipt recording used to be gated on the transitioning call only,
//     permanently stranding a delivery if the process crashed between the
//     two writes).

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	messaging "github.com/hollis-labs/go-messaging"
	"github.com/hollis-labs/go-messaging/delivery"
	"github.com/hollis-labs/go-messaging/deliverytest"

	"github.com/hollis-labs/tether/internal/store"
)

func TestDeliveryStore_Contract(t *testing.T) {
	deliverytest.RunStoreContract(t, func(t *testing.T) deliverytest.Harness {
		db, err := store.Open(filepath.Join(t.TempDir(), "delivery.db"))
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		t.Cleanup(func() { db.Close() })
		clock := deliverytest.NewFakeClock(time.Now())
		// The contract suite needs deterministic clock control, so this
		// harness constructs its own delivery.SQLiteStore with a FakeClock
		// bound to db.DB() rather than using Store.DeliveryStore() (which
		// always uses the real wall clock) -- same *sql.DB, same schema
		// (already applied by store.Open), different Store instance for
		// clock injection only.
		return deliverytest.Harness{
			Store: delivery.NewSQLiteStore(db.DB(), delivery.WithSQLiteClock(clock)),
			Clock: clock,
		}
	})
}

func TestDeliveryStore_SurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "restart.db")
	ctx := context.Background()

	db1, err := store.Open(path)
	if err != nil {
		t.Fatalf("open 1: %v", err)
	}
	res, err := db1.DeliveryStore().Enqueue(ctx, delivery.EnqueueRequest{
		From:       addr("sender"),
		Recipients: []delivery.RecipientTarget{{Address: addr("recipient")}},
		Kind:       messaging.MsgKindNotice,
		Payload:    []byte("hello"),
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	claim, err := db1.DeliveryStore().Claim(ctx, delivery.ClaimRequest{
		DeliveryID:    res.Deliveries[0].ID,
		Holder:        "worker-1",
		LeaseDuration: time.Minute,
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, _, err := db1.DeliveryStore().Ack(ctx, delivery.AckRequest{
		Lease: delivery.LeaseRef{DeliveryID: claim.Attempt.DeliveryID, AttemptID: claim.Attempt.ID, LeaseToken: claim.Attempt.LeaseToken},
		Stage: delivery.StageHostAccepted,
	}); err != nil {
		t.Fatalf("ack host_accepted: %v", err)
	}
	if err := db1.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Reopen against the SAME file path with a brand-new *sql.DB/Store.
	db2, err := store.Open(path)
	if err != nil {
		t.Fatalf("open 2: %v", err)
	}
	defer db2.Close()

	msg, err := db2.DeliveryStore().GetMessage(ctx, res.Message.ID)
	if err != nil {
		t.Fatalf("get message after restart: %v", err)
	}
	if string(msg.Payload) != "hello" {
		t.Fatalf("payload not preserved across restart: %q", msg.Payload)
	}
	del, err := db2.DeliveryStore().GetDelivery(ctx, res.Deliveries[0].ID)
	if err != nil {
		t.Fatalf("get delivery after restart: %v", err)
	}
	if del.AttemptCount != 1 {
		t.Fatalf("expected attempt count 1 to survive restart, got %d", del.AttemptCount)
	}
	receipts, err := db2.DeliveryStore().Receipts(ctx, res.Deliveries[0].ID)
	if err != nil {
		t.Fatalf("receipts after restart: %v", err)
	}
	var sawHostAccepted bool
	for _, r := range receipts {
		if r.Stage == delivery.StageHostAccepted {
			sawHostAccepted = true
		}
	}
	if !sawHostAccepted {
		t.Fatalf("expected host_accepted receipt to survive restart, got %+v", receipts)
	}
}

// TestDeliveryStore_ConcurrentClaimsAcrossIndependentConnections uses TWO
// separate *sql.DB handles against the SAME database file -- not two
// goroutines sharing one connection pool -- to prove the library's claim
// exclusivity holds at the file/SQLite level, matching the real Tether
// topology where the daemon process and the `mux mcp` subprocess each open
// their own independent connection to the same ~/.tether/state/tether.db
// (T01 contract §2.7). A plain in-process Go race test would not exercise
// this: it would only prove goroutines-sharing-one-*sql.DB behave, which
// Tether's own MaxOpenConns=1 already trivially serializes.
func TestDeliveryStore_ConcurrentClaimsAcrossIndependentConnections(t *testing.T) {
	path := filepath.Join(t.TempDir(), "concurrent.db")

	// First connection creates the schema and enqueues the message.
	seed, err := store.Open(path)
	if err != nil {
		t.Fatalf("open seed: %v", err)
	}
	res, err := seed.DeliveryStore().Enqueue(context.Background(), delivery.EnqueueRequest{
		From:       addr("sender"),
		Recipients: []delivery.RecipientTarget{{Address: addr("recipient")}},
		Kind:       messaging.MsgKindNotice,
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("close seed: %v", err)
	}

	const n = 6
	dbs := make([]*store.Store, n)
	for i := range n {
		db, err := store.Open(path)
		if err != nil {
			t.Fatalf("open connection %d: %v", i, err)
		}
		defer db.Close()
		dbs[i] = db
	}

	results := make([]error, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			_, err := dbs[i].DeliveryStore().Claim(context.Background(), delivery.ClaimRequest{
				DeliveryID:    res.Deliveries[0].ID,
				Holder:        "worker",
				LeaseDuration: time.Minute,
				Nowait:        true,
			})
			results[i] = err
		}(i)
	}
	wg.Wait()

	successes, alreadyClaimed := 0, 0
	for i, err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, delivery.ErrAlreadyClaimed):
			alreadyClaimed++
		default:
			t.Fatalf("connection %d: unexpected error: %v", i, err)
		}
	}
	if successes != 1 {
		t.Fatalf("expected exactly 1 successful claim across %d independent connections, got %d (alreadyClaimed=%d)", n, successes, alreadyClaimed)
	}
	if alreadyClaimed != n-1 {
		t.Fatalf("expected the remaining %d connections to observe ErrAlreadyClaimed, got %d", n-1, alreadyClaimed)
	}
}

// TestDeliveryStore_CrashInjection_PartialEnqueueRollsBack injects a failure
// after the message row is inserted but before the transaction commits, and
// proves NOTHING committed -- no orphaned message, no delivery row. This is
// "persistence-before-notify": a half-finished Enqueue must never be
// observable, so a caller can trust that any Enqueue result it actually
// received corresponds to fully durable state.
func TestDeliveryStore_CrashInjection_PartialEnqueueRollsBack(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "crash-enqueue.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	injected := errors.New("injected crash after message insert")
	crashingStore := delivery.NewSQLiteStore(db.DB(), delivery.WithSQLiteMutationHook(func(step delivery.SQLiteMutationStep) error {
		if step == delivery.SQLiteStepAfterMessageInsert {
			return injected
		}
		return nil
	}))

	_, err = crashingStore.Enqueue(context.Background(), delivery.EnqueueRequest{
		From:       addr("sender"),
		Recipients: []delivery.RecipientTarget{{Address: addr("recipient")}},
		Kind:       messaging.MsgKindNotice,
	})
	if err == nil {
		t.Fatalf("expected the injected crash to surface as an error")
	}

	// A fresh, un-hooked store over the SAME db must see no trace of the
	// failed enqueue -- proves the transaction actually rolled back rather
	// than partially committing.
	clean := delivery.NewSQLiteStore(db.DB())
	all, err := clean.ListDeliveries(context.Background(), delivery.Filter{})
	if err != nil {
		t.Fatalf("list deliveries: %v", err)
	}
	if len(all) != 0 {
		t.Fatalf("expected zero deliveries after a rolled-back enqueue, got %d: %+v", len(all), all)
	}
}

// TestDeliveryStore_CrashInjection_PartialClaimRollsBack does the same for
// Claim -- injects a failure after the attempt row is inserted (but before
// the lease/delivery-status update + commit), and proves the delivery
// remains pending/unclaimed rather than stuck half-claimed. This is the
// "durable handoff recovery" half of acceptance #3: a crash mid-claim must
// leave the obligation cleanly reclaimable, not lost or wedged.
func TestDeliveryStore_CrashInjection_PartialClaimRollsBack(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "crash-claim.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	plain := delivery.NewSQLiteStore(db.DB())
	res, err := plain.Enqueue(context.Background(), delivery.EnqueueRequest{
		From:       addr("sender"),
		Recipients: []delivery.RecipientTarget{{Address: addr("recipient")}},
		Kind:       messaging.MsgKindNotice,
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	injected := errors.New("injected crash after attempt insert")
	crashingStore := delivery.NewSQLiteStore(db.DB(), delivery.WithSQLiteMutationHook(func(step delivery.SQLiteMutationStep) error {
		if step == delivery.SQLiteStepAfterAttemptInsert {
			return injected
		}
		return nil
	}))
	_, err = crashingStore.Claim(context.Background(), delivery.ClaimRequest{
		DeliveryID:    res.Deliveries[0].ID,
		Holder:        "worker-1",
		LeaseDuration: time.Minute,
	})
	if err == nil {
		t.Fatalf("expected the injected crash to surface as an error")
	}

	// A fresh claim over the same delivery, from an un-hooked store, must
	// succeed cleanly -- the failed claim left no dangling lease behind.
	claim, err := plain.Claim(context.Background(), delivery.ClaimRequest{
		DeliveryID:    res.Deliveries[0].ID,
		Holder:        "worker-2",
		LeaseDuration: time.Minute,
	})
	if err != nil {
		t.Fatalf("expected a clean reclaim after the rolled-back crash, got: %v", err)
	}
	if claim.Attempt.Holder != "worker-2" {
		t.Fatalf("expected worker-2 to win the reclaim, got holder=%q", claim.Attempt.Holder)
	}
	del, err := plain.GetDelivery(context.Background(), res.Deliveries[0].ID)
	if err != nil {
		t.Fatalf("get delivery: %v", err)
	}
	if del.AttemptCount != 1 {
		t.Fatalf("expected exactly 1 real attempt recorded (the rolled-back one must not count), got %d", del.AttemptCount)
	}
}

func TestDeliveryBackedSend_CreatesRealDeliveryObligation(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "send.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	ms := db.MessagingStore()
	sent, err := ms.Send(context.Background(), messaging.Envelope{
		From:    addr("alice"),
		To:      addr("bob"),
		Kind:    messaging.MsgKindNotice,
		Payload: []byte("hi bob"),
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if sent.ID == "" {
		t.Fatalf("expected a minted ID")
	}

	// The message content is unchanged/readable exactly as before (backward
	// compatibility -- Get/messages table untouched by this decorator).
	got, err := ms.Get(context.Background(), sent.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(got.Payload) != "hi bob" {
		t.Fatalf("payload mismatch: %q", got.Payload)
	}

	// AND a real delivery-core obligation now exists, sharing the same
	// message ID (T03's actual point: adopt the shared store rather than
	// leaving delivered_at/consumed_at as the only signal).
	msg, err := db.DeliveryStore().GetMessage(context.Background(), delivery.MessageID(sent.ID))
	if err != nil {
		t.Fatalf("expected a delivery-core Message sharing messages.id, got: %v", err)
	}
	if string(msg.Payload) != "hi bob" {
		t.Fatalf("delivery-core payload mismatch: %q", msg.Payload)
	}
	deliveries, err := db.DeliveryStore().ListDeliveries(context.Background(), delivery.Filter{MessageID: delivery.MessageID(sent.ID)})
	if err != nil {
		t.Fatalf("list deliveries: %v", err)
	}
	if len(deliveries) != 1 {
		t.Fatalf("expected exactly one delivery obligation for the send, got %d", len(deliveries))
	}
	if deliveries[0].Status != delivery.DeliveryPending {
		t.Fatalf("expected a freshly enqueued delivery to be pending, got %q", deliveries[0].Status)
	}
}

func TestDeliveryBackedConsume_RecordsReceiptsIdempotently(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "consume.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	ms := db.MessagingStore()
	sent, err := ms.Send(context.Background(), messaging.Envelope{From: addr("alice"), To: addr("bob"), Kind: messaging.MsgKindNotice})
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	if err := ms.Consume(context.Background(), sent.ID, addr("bob")); err != nil {
		t.Fatalf("consume: %v", err)
	}

	var deliveryID sql.NullString
	if err := db.DB().QueryRow(`SELECT delivery_id FROM messages WHERE id=?`, sent.ID).Scan(&deliveryID); err != nil {
		t.Fatalf("read delivery_id: %v", err)
	}
	if !deliveryID.Valid {
		t.Fatalf("expected delivery_id to be recorded on send")
	}
	del, err := db.DeliveryStore().GetDelivery(context.Background(), delivery.DeliveryID(deliveryID.String))
	if err != nil {
		t.Fatalf("get delivery: %v", err)
	}
	if del.Status != delivery.DeliveryDelivered {
		t.Fatalf("expected consume to drive the delivery to 'delivered', got %q", del.Status)
	}
	receipts, err := db.DeliveryStore().Receipts(context.Background(), delivery.DeliveryID(deliveryID.String))
	if err != nil {
		t.Fatalf("receipts: %v", err)
	}
	stages := map[delivery.ReceiptStage]bool{}
	for _, r := range receipts {
		stages[r.Stage] = true
	}
	for _, want := range []delivery.ReceiptStage{delivery.StagePersisted, delivery.StageHostAccepted, delivery.StageTurnSubmitted, delivery.StageConsumed} {
		if !stages[want] {
			t.Fatalf("expected receipt stage %q to be recorded, got stages %+v", want, stages)
		}
	}

	// Idempotent replay: consuming again must not error, must not re-claim
	// (the delivery is already terminal), and must not duplicate receipts.
	if err := ms.Consume(context.Background(), sent.ID, addr("bob")); err != nil {
		t.Fatalf("idempotent consume: %v", err)
	}
	receiptsAfter, err := db.DeliveryStore().Receipts(context.Background(), delivery.DeliveryID(deliveryID.String))
	if err != nil {
		t.Fatalf("receipts after replay: %v", err)
	}
	if len(receiptsAfter) != len(receipts) {
		t.Fatalf("expected the idempotent replay to add no new receipts: before=%d after=%d", len(receipts), len(receiptsAfter))
	}
}

// TestDeliveryBackedConsume_RecoversReceiptsAfterCrashBeforeAck is the
// CW-20260907-0033 regression test. Consume's two writes (messages.
// consumed_at, delivery-core receipts) are against separate stores with no
// shared transaction; if the process crashed/failed between them, the old
// code (gating recordConsumedReceipts on "this call transitioned
// consumed_at") could NEVER retry the receipt recording -- the row is
// already consumed, so every future call takes the idempotent no-op branch
// and returns immediately, permanently stranding the delivery in a
// non-terminal state. This test manually reproduces exactly that
// intermediate state (consumed_at committed, delivery-core untouched) and
// asserts a later Consume call -- the caller's own retry-after-failure,
// which Consume's documented idempotency already invites -- completes the
// missing receipts and drives the delivery to Delivered.
func TestDeliveryBackedConsume_RecoversReceiptsAfterCrashBeforeAck(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "consume-crash-recovery.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	ms := db.MessagingStore()
	sent, err := ms.Send(context.Background(), messaging.Envelope{From: addr("alice"), To: addr("bob"), Kind: messaging.MsgKindNotice})
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	// Simulate the crash window: commit consumed_at directly (bypassing
	// Consume entirely), leaving the delivery-core row exactly as Enqueue
	// left it -- no claim, no receipts beyond StagePersisted.
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.DB().Exec(`UPDATE messages SET consumed_at=? WHERE id=?`, now, sent.ID); err != nil {
		t.Fatalf("simulate crash-window consumed_at: %v", err)
	}

	var deliveryID sql.NullString
	if err := db.DB().QueryRow(`SELECT delivery_id FROM messages WHERE id=?`, sent.ID).Scan(&deliveryID); err != nil {
		t.Fatalf("read delivery_id: %v", err)
	}
	if !deliveryID.Valid {
		t.Fatalf("expected delivery_id to be recorded on send")
	}
	del, err := db.DeliveryStore().GetDelivery(context.Background(), delivery.DeliveryID(deliveryID.String))
	if err != nil {
		t.Fatalf("get delivery: %v", err)
	}
	if del.Status == delivery.DeliveryDelivered {
		t.Fatalf("test setup invalid: delivery must NOT already be terminal before the recovery call")
	}

	// The recovery call: an idempotent replay from the caller's perspective
	// (consumed_at is already set), but the first real chance to record
	// delivery-core receipts.
	if err := ms.Consume(context.Background(), sent.ID, addr("bob")); err != nil {
		t.Fatalf("recovery consume: %v", err)
	}

	del, err = db.DeliveryStore().GetDelivery(context.Background(), delivery.DeliveryID(deliveryID.String))
	if err != nil {
		t.Fatalf("get delivery after recovery: %v", err)
	}
	if del.Status != delivery.DeliveryDelivered {
		t.Fatalf("expected the recovery call to drive the delivery to 'delivered', got %q -- the stranded-delivery bug (CW-20260907-0033) is not fixed", del.Status)
	}
	receipts, err := db.DeliveryStore().Receipts(context.Background(), delivery.DeliveryID(deliveryID.String))
	if err != nil {
		t.Fatalf("receipts: %v", err)
	}
	stages := map[delivery.ReceiptStage]bool{}
	for _, r := range receipts {
		stages[r.Stage] = true
	}
	for _, want := range []delivery.ReceiptStage{delivery.StageHostAccepted, delivery.StageTurnSubmitted, delivery.StageConsumed} {
		if !stages[want] {
			t.Fatalf("expected the recovery call to record receipt stage %q, got stages %+v", want, stages)
		}
	}
}

// TestDeliveryBackedConsume_ReusesActiveLeaseAcrossRestart is a second,
// remaining instance of CW-20260907-0033, found by independent review after
// the crash-before-ack fix above landed: recordConsumedReceipts always
// requested a brand-new Claim, which go-messaging correctly refuses
// (ErrAlreadyClaimed) whenever ANY lease on the delivery is still active --
// including one Tether's own wake pump already opened and deliberately left
// open "awaiting consumption" (see internal/app/wake.go's attemptWake). The
// old code logged and discarded that error, leaving the delivery stuck
// non-terminal at whatever stage the still-valid lease was at, for every
// restart point: right after the lease was acquired, or after either
// partial acknowledgement.
//
// The fix (migration 0022) only reuses a lease whose
// pending_receipt_attempt_id/pending_receipt_lease_token marker matches --
// written here via SetPendingReceiptLease right after claiming, exactly as
// leaseForConsumedReceipts's own fallback-Claim branch does in production,
// simulating that a real prior (interrupted) Consume call reached that same
// point before whatever crashed/restarted it.
func TestDeliveryBackedConsume_ReusesActiveLeaseAcrossRestart(t *testing.T) {
	for _, stage := range []delivery.ReceiptStage{delivery.StageLeaseAcquired, delivery.StageHostAccepted, delivery.StageTurnSubmitted} {
		t.Run(string(stage), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "review.db")
			db, err := store.Open(path)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			bob := addr("bob")
			alice := addr("alice")
			msg, err := db.MessagingStore().Send(context.Background(), messaging.Envelope{From: alice, To: bob, Kind: messaging.MsgKindNotice})
			if err != nil {
				t.Fatalf("send: %v", err)
			}
			// Simulate consumed_at already committed by an earlier call
			// (the scenario the first CW-20260907-0033 fix already
			// handles) -- this test is specifically about what happens
			// when that earlier call also opened a still-active lease
			// (via a prior Consume attempt, or Tether's own wake) before
			// whatever interrupted it.
			if _, err := db.DB().Exec(`UPDATE messages SET consumed_at=? WHERE id=?`, time.Now().UTC().Format(time.RFC3339Nano), msg.ID); err != nil {
				t.Fatalf("simulate consumed_at: %v", err)
			}
			id, ok, err := db.DeliveryIDForMessage(context.Background(), msg.ID)
			if err != nil || !ok {
				t.Fatalf("delivery id: ok=%v err=%v", ok, err)
			}
			claim, err := db.DeliveryStore().Claim(context.Background(), delivery.ClaimRequest{
				DeliveryID: delivery.DeliveryID(id), Holder: bob.URN(), LeaseDuration: time.Minute, Nowait: true,
			})
			if err != nil {
				t.Fatalf("claim: %v", err)
			}
			if err := db.SetPendingReceiptLease(context.Background(), msg.ID, claim.Attempt.ID, claim.Attempt.LeaseToken); err != nil {
				t.Fatalf("mark pending receipt lease: %v", err)
			}
			lease := delivery.LeaseRef{DeliveryID: claim.Attempt.DeliveryID, AttemptID: claim.Attempt.ID, LeaseToken: claim.Attempt.LeaseToken, BindingGeneration: claim.Attempt.BindingGeneration}
			if stage != delivery.StageLeaseAcquired {
				if _, _, err := db.DeliveryStore().Ack(context.Background(), delivery.AckRequest{Lease: lease, Stage: delivery.StageHostAccepted}); err != nil {
					t.Fatalf("ack host_accepted: %v", err)
				}
			}
			if stage == delivery.StageTurnSubmitted {
				if _, _, err := db.DeliveryStore().Ack(context.Background(), delivery.AckRequest{Lease: lease, Stage: delivery.StageTurnSubmitted}); err != nil {
					t.Fatalf("ack turn_submitted: %v", err)
				}
			}
			if err := db.Close(); err != nil {
				t.Fatalf("close: %v", err)
			}

			// Reopen (simulating a restart/reconnect) and retry Consume,
			// same as the caller's own documented-idempotent retry.
			db, err = store.Open(path)
			if err != nil {
				t.Fatalf("reopen: %v", err)
			}
			defer db.Close()
			if err := db.MessagingStore().Consume(context.Background(), msg.ID, bob); err != nil {
				t.Fatalf("replay consume: %v", err)
			}

			after, err := db.DeliveryStore().GetDelivery(context.Background(), delivery.DeliveryID(id))
			if err != nil {
				t.Fatalf("get delivery: %v", err)
			}
			// AttemptCount must stay 1: a real fix REUSES the existing
			// attempt/lease rather than acquiring a new one (Claim
			// increments attempt_count every time it succeeds) -- this
			// distinguishes "the marked lease was actually reused" from a
			// hypothetical alternate implementation that reaches Delivered
			// via some other means (e.g. force-clearing the old lease and
			// claiming fresh), which would leave a stray second attempt.
			if after.AttemptCount != 1 {
				t.Fatalf("attempt count = %d after replay, want 1 -- Consume claimed a NEW attempt instead of reusing the marked one", after.AttemptCount)
			}
			if after.Status != delivery.DeliveryDelivered {
				t.Fatalf("Consume replay reported success but delivery stayed %q at prior stage %s -- the still-active, marked lease was not reused (CW-20260907-0033 is not fully fixed)", after.Status, stage)
			}
		})
	}
}

// TestDeliveryBackedConsume_DoesNotHijackAnIndependentlyClaimedLease is the
// regression test for the hijack risk a second independent reviewer found
// in this fix's own first version: reusing ANY active lease (not just a
// marked, Consume-owned one) could let Consume finish -- or a later
// legitimate Nack from the real claimant downgrade -- work an entirely
// independent external process still had in flight via T07's
// published-local bridge surface (POST /messages/{id}/claim, which is
// "the counterpart to AttemptWake's internal Claim for a Tether-pushed
// wake" per its own doc comment, and defaults its `holder` to the exact
// same asserted recipient URN Consume itself uses -- so Holder identity
// alone cannot distinguish the two). This test claims the delivery exactly
// the way that bridge does (ClaimMessageDelivery, no pending-receipt
// marker) and asserts a subsequent Consume call leaves that claim
// completely untouched.
func TestDeliveryBackedConsume_DoesNotHijackAnIndependentlyClaimedLease(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "no-hijack.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	bob := addr("bob")
	alice := addr("alice")
	msg, err := db.MessagingStore().Send(ctx, messaging.Envelope{From: alice, To: bob, Kind: messaging.MsgKindNotice})
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	// An independent external claimant (a T07 bridge) claims the delivery
	// on its own initiative -- exactly ClaimMessageDelivery's own call
	// shape, using the same default holder (the recipient's own URN) a
	// real bridge call would use. Crucially: no SetPendingReceiptLease
	// call, since the bridge path never makes one.
	_, bridgeLease, err := db.ClaimMessageDelivery(ctx, msg.ID, bob, bob.URN(), time.Minute)
	if err != nil {
		t.Fatalf("bridge claim: %v", err)
	}

	// The recipient (or anything asserting its identity) calls Consume.
	// Before this fix's hijack correction, this would have found the
	// bridge's active lease, driven it straight to Consumed, and stolen it
	// out from under the bridge's own still-in-flight work.
	if err := db.MessagingStore().Consume(ctx, msg.ID, bob); err != nil {
		t.Fatalf("consume: %v", err)
	}

	del, err := db.DeliveryStore().GetDelivery(ctx, bridgeLease.DeliveryID)
	if err != nil {
		t.Fatalf("get delivery: %v", err)
	}
	if del.Status != delivery.DeliveryLeased {
		t.Fatalf("delivery status = %q after Consume, want still leased -- Consume hijacked an independent claimant's active lease", del.Status)
	}
	if del.ActiveAttemptID != bridgeLease.AttemptID || del.ActiveLeaseToken != bridgeLease.LeaseToken {
		t.Fatalf("active attempt/lease changed after Consume (attempt=%s token=%s), want the bridge's original attempt=%s token=%s untouched",
			del.ActiveAttemptID, del.ActiveLeaseToken, bridgeLease.AttemptID, bridgeLease.LeaseToken)
	}

	// The bridge can still legitimately finish its own work afterward.
	if _, _, err := db.AckMessageDelivery(ctx, bridgeLease, delivery.StageConsumed); err != nil {
		t.Fatalf("bridge's own ack(consumed) after Consume ran: %v", err)
	}
	final, err := db.DeliveryStore().GetDelivery(ctx, bridgeLease.DeliveryID)
	if err != nil {
		t.Fatalf("get delivery: %v", err)
	}
	if final.Status != delivery.DeliveryDelivered {
		t.Fatalf("delivery status = %q after the bridge's own ack(consumed), want delivered", final.Status)
	}
}

// TestDeliveryBackedConsume_ClaimsFreshFromRetryScheduled covers the one
// non-Leased, non-Pending status leaseForConsumedReceipts's reuse-gate
// (`del.Status == delivery.DeliveryLeased`) must correctly fall through
// for: a delivery that was Nacked retryable (e.g. a prior wake attempt
// failed and released it) has NO active lease at all -- Nack clears
// ActiveAttemptID/ActiveLeaseToken -- so Consume must claim fresh here,
// exactly as it already does for a plain Pending delivery.
func TestDeliveryBackedConsume_ClaimsFreshFromRetryScheduled(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "retry-scheduled.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	bob := addr("bob")
	alice := addr("alice")
	msg, err := db.MessagingStore().Send(ctx, messaging.Envelope{From: alice, To: bob, Kind: messaging.MsgKindNotice})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	id, ok, err := db.DeliveryIDForMessage(ctx, msg.ID)
	if err != nil || !ok {
		t.Fatalf("delivery id: ok=%v err=%v", ok, err)
	}
	claim, err := db.DeliveryStore().Claim(ctx, delivery.ClaimRequest{
		DeliveryID: delivery.DeliveryID(id), Holder: "some-host", LeaseDuration: time.Minute, Nowait: true,
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	lease := delivery.LeaseRef{DeliveryID: claim.Attempt.DeliveryID, AttemptID: claim.Attempt.ID, LeaseToken: claim.Attempt.LeaseToken, BindingGeneration: claim.Attempt.BindingGeneration}
	if _, _, err := db.DeliveryStore().Nack(ctx, delivery.NackRequest{Lease: lease, Retryable: true, Error: "simulated transient failure"}); err != nil {
		t.Fatalf("nack: %v", err)
	}
	del, err := db.DeliveryStore().GetDelivery(ctx, delivery.DeliveryID(id))
	if err != nil {
		t.Fatalf("get delivery: %v", err)
	}
	if del.Status != delivery.DeliveryRetryScheduled {
		t.Fatalf("test setup invalid: status = %q, want retry_scheduled", del.Status)
	}

	if err := db.MessagingStore().Consume(ctx, msg.ID, bob); err != nil {
		t.Fatalf("consume: %v", err)
	}
	after, err := db.DeliveryStore().GetDelivery(ctx, delivery.DeliveryID(id))
	if err != nil {
		t.Fatalf("get delivery after consume: %v", err)
	}
	if after.Status != delivery.DeliveryDelivered {
		t.Fatalf("status after consume = %q, want delivered -- Consume must claim fresh from retry_scheduled", after.Status)
	}
}

func TestDeliveryBackedConsume_PreT03MessageHasNoDeliveryID(t *testing.T) {
	// A message with no delivery_id (e.g. imported from before T03, or sent
	// through a path that predates this decorator) must still Consume
	// successfully -- delivery-core recording is additive, never a
	// precondition for the base contract.
	db, err := store.Open(filepath.Join(t.TempDir(), "no-delivery-id.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.DB().Exec(
		`INSERT INTO messages (id, kind, from_urn, to_urn, created_at) VALUES (?, ?, ?, ?, ?)`,
		"legacy-msg-1", string(messaging.MsgKindNotice), addr("alice").URN(), addr("bob").URN(), now,
	); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}

	if err := db.MessagingStore().Consume(context.Background(), "legacy-msg-1", addr("bob")); err != nil {
		t.Fatalf("consume a no-delivery-id row must still succeed: %v", err)
	}
}
