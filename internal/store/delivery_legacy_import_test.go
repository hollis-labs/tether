package store_test

// delivery_legacy_import_test.go — T03 acceptance #2 evidence. All tests run
// against a fresh t.TempDir() fixture database, never live ~/.tether data,
// per the execution contract's explicit "rollback/recovery instructions use
// fixture copies" requirement.

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/hollis-labs/go-messaging/delivery"

	"github.com/hollis-labs/tether/internal/store"
)

// seedLegacyMessage inserts a `messages` row directly, bypassing Send/the
// delivery decorator entirely -- simulating a row that was written before
// T03 existed (delivery_id is naturally NULL, since the column didn't exist
// when such a row would originally have been written).
func seedLegacyMessage(t *testing.T, db *sql.DB, id string, deliveredAt, consumedAt, canceledAt, groupURN *string) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := db.Exec(
		`INSERT INTO messages (id, kind, from_urn, to_urn, payload, created_at, delivered_at, consumed_at, canceled_at, group_urn)
		 VALUES (?, 'notice', ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, "msg://agent/tether/alice", "msg://agent/tether/bob", "payload-"+id, now,
		nullableStr(deliveredAt), nullableStr(consumedAt), nullableStr(canceledAt), nullableStr(groupURN),
	)
	if err != nil {
		t.Fatalf("seed legacy message %s: %v", id, err)
	}
}

func nullableStr(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

func strPtr(s string) *string { return &s }

func TestLegacyImport_NeverDelivered_ImportsAsPending(t *testing.T) {
	db, err := store.Open(pathFor(t, "import-pending.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	seedLegacyMessage(t, db.DB(), "msg-never-delivered", nil, nil, nil, nil)

	res, err := store.ImportLegacyMessagesIntoDelivery(context.Background(), db.DB(), db.DeliveryStore(), "")
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if res.Imported != 1 || res.ImportedPending != 1 {
		t.Fatalf("expected 1 imported/1 pending, got %+v", res)
	}

	var deliveryID sql.NullString
	if err := db.DB().QueryRow(`SELECT delivery_id FROM messages WHERE id=?`, "msg-never-delivered").Scan(&deliveryID); err != nil {
		t.Fatalf("read delivery_id: %v", err)
	}
	if !deliveryID.Valid {
		t.Fatalf("expected a delivery_id mapping to be recorded")
	}
	del, err := db.DeliveryStore().GetDelivery(context.Background(), delivery.DeliveryID(deliveryID.String))
	if err != nil {
		t.Fatalf("get delivery: %v", err)
	}
	if del.Status != delivery.DeliveryPending {
		t.Fatalf("expected pending status, got %q", del.Status)
	}
}

func TestLegacyImport_AmbiguousDelivered_HeldByDefault(t *testing.T) {
	db, err := store.Open(pathFor(t, "import-ambiguous.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	delivered := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	seedLegacyMessage(t, db.DB(), "msg-ambiguous", &delivered, nil, nil, nil)

	res, err := store.ImportLegacyMessagesIntoDelivery(context.Background(), db.DB(), db.DeliveryStore(), store.ImportHoldAmbiguousDelivered)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if res.Imported != 1 || res.ImportedHeld != 1 {
		t.Fatalf("expected 1 imported/1 held, got %+v", res)
	}

	var deliveryID string
	if err := db.DB().QueryRow(`SELECT delivery_id FROM messages WHERE id=?`, "msg-ambiguous").Scan(&deliveryID); err != nil {
		t.Fatalf("read delivery_id: %v", err)
	}
	del, err := db.DeliveryStore().GetDelivery(context.Background(), delivery.DeliveryID(deliveryID))
	if err != nil {
		t.Fatalf("get delivery: %v", err)
	}
	if del.Status != delivery.DeliveryDeadLettered {
		t.Fatalf("expected the ambiguous row to be held as dead-lettered by default, got %q", del.Status)
	}
	if del.DeadLetterReason == "" {
		t.Fatalf("expected a dead-letter reason explaining the ambiguity, got empty")
	}
}

func TestLegacyImport_AmbiguousDelivered_ReplayOptIn(t *testing.T) {
	db, err := store.Open(pathFor(t, "import-replay.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	delivered := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	seedLegacyMessage(t, db.DB(), "msg-replay", &delivered, nil, nil, nil)

	res, err := store.ImportLegacyMessagesIntoDelivery(context.Background(), db.DB(), db.DeliveryStore(), store.ImportReplayAmbiguousDelivered)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if res.Imported != 1 || res.ImportedReplay != 1 {
		t.Fatalf("expected 1 imported/1 replay, got %+v", res)
	}

	var deliveryID string
	if err := db.DB().QueryRow(`SELECT delivery_id FROM messages WHERE id=?`, "msg-replay").Scan(&deliveryID); err != nil {
		t.Fatalf("read delivery_id: %v", err)
	}
	del, err := db.DeliveryStore().GetDelivery(context.Background(), delivery.DeliveryID(deliveryID))
	if err != nil {
		t.Fatalf("get delivery: %v", err)
	}
	if del.Status != delivery.DeliveryPending {
		t.Fatalf("expected the explicit replay opt-in to leave the row pending (redeliverable), got %q", del.Status)
	}
}

func TestLegacyImport_ConfirmedComplete_ReplaysHistoricalReceipts(t *testing.T) {
	db, err := store.Open(pathFor(t, "import-complete.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	delivered := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339Nano)
	consumed := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	seedLegacyMessage(t, db.DB(), "msg-complete", &delivered, &consumed, nil, nil)

	res, err := store.ImportLegacyMessagesIntoDelivery(context.Background(), db.DB(), db.DeliveryStore(), "")
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if res.Imported != 1 || res.ImportedComplete != 1 {
		t.Fatalf("expected 1 imported/1 complete, got %+v", res)
	}

	var deliveryID string
	if err := db.DB().QueryRow(`SELECT delivery_id FROM messages WHERE id=?`, "msg-complete").Scan(&deliveryID); err != nil {
		t.Fatalf("read delivery_id: %v", err)
	}
	del, err := db.DeliveryStore().GetDelivery(context.Background(), delivery.DeliveryID(deliveryID))
	if err != nil {
		t.Fatalf("get delivery: %v", err)
	}
	if del.Status != delivery.DeliveryDelivered {
		t.Fatalf("expected confirmed-complete rows to import as delivered, got %q", del.Status)
	}
	receipts, err := db.DeliveryStore().Receipts(context.Background(), delivery.DeliveryID(deliveryID))
	if err != nil {
		t.Fatalf("receipts: %v", err)
	}
	stages := map[delivery.ReceiptStage]time.Time{}
	for _, r := range receipts {
		stages[r.Stage] = r.At
	}
	wantDelivered, _ := time.Parse(time.RFC3339Nano, delivered)
	wantConsumed, _ := time.Parse(time.RFC3339Nano, consumed)
	if !stages[delivery.StageConsumed].Equal(wantConsumed) {
		t.Fatalf("expected consumed receipt backdated to %v, got %v", wantConsumed, stages[delivery.StageConsumed])
	}
	if !stages[delivery.StageHostAccepted].Equal(wantDelivered) {
		t.Fatalf("expected host_accepted receipt backdated to %v, got %v", wantDelivered, stages[delivery.StageHostAccepted])
	}

	// Message content itself was never touched.
	var payload string
	if err := db.DB().QueryRow(`SELECT payload FROM messages WHERE id=?`, "msg-complete").Scan(&payload); err != nil {
		t.Fatalf("read payload: %v", err)
	}
	if payload != "payload-msg-complete" {
		t.Fatalf("message content must be untouched by import, got %q", payload)
	}
}

func TestLegacyImport_SkipsCanceledAndGroupRows(t *testing.T) {
	db, err := store.Open(pathFor(t, "import-skips.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	canceledAt := time.Now().UTC().Format(time.RFC3339Nano)
	seedLegacyMessage(t, db.DB(), "msg-canceled", nil, nil, &canceledAt, nil)
	seedLegacyMessage(t, db.DB(), "msg-group", nil, nil, nil, strPtr("msg://group/tether/grp_x"))

	res, err := store.ImportLegacyMessagesIntoDelivery(context.Background(), db.DB(), db.DeliveryStore(), "")
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if res.Imported != 0 {
		t.Fatalf("expected 0 imported (both rows skipped), got %+v", res)
	}
	if res.SkippedCanceled != 1 {
		t.Fatalf("expected 1 skipped-canceled, got %+v", res)
	}
	if res.SkippedGroup != 1 {
		t.Fatalf("expected 1 skipped-group, got %+v", res)
	}

	var deliveryID sql.NullString
	if err := db.DB().QueryRow(`SELECT delivery_id FROM messages WHERE id=?`, "msg-canceled").Scan(&deliveryID); err != nil {
		t.Fatalf("read: %v", err)
	}
	if deliveryID.Valid {
		t.Fatalf("a skipped canceled row must not get a delivery_id")
	}
}

func TestLegacyImport_IdempotentOnRerun(t *testing.T) {
	db, err := store.Open(pathFor(t, "import-idempotent.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	seedLegacyMessage(t, db.DB(), "msg-a", nil, nil, nil, nil)

	first, err := store.ImportLegacyMessagesIntoDelivery(context.Background(), db.DB(), db.DeliveryStore(), "")
	if err != nil {
		t.Fatalf("first import: %v", err)
	}
	if first.Imported != 1 {
		t.Fatalf("expected 1 imported on first run, got %+v", first)
	}

	second, err := store.ImportLegacyMessagesIntoDelivery(context.Background(), db.DB(), db.DeliveryStore(), "")
	if err != nil {
		t.Fatalf("second import: %v", err)
	}
	if second.Imported != 0 {
		t.Fatalf("expected 0 newly imported on rerun (already-mapped rows are skipped by the WHERE delivery_id IS NULL query), got %+v", second)
	}
}

func pathFor(t *testing.T, name string) string {
	t.Helper()
	return t.TempDir() + "/" + name
}
