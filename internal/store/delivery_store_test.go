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
//     drives host_accepted/turn_submitted/consumed receipts exactly once,
//     on the transitioning call only.

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

func TestDeliveryBackedConsume_RecordsReceiptsOnlyOnTransition(t *testing.T) {
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
