package store_test

// retention_test.go — T09 (messaging vNext, CW-20260906-0040) acceptance
// #3 evidence: "retention has explicit behavior for pending obligations
// and no silent deletion." Each test below proves one concrete case of
// that: a pending, leased, retry-scheduled or dead-lettered delivery is
// refused (not silently skipped, not silently purged); a terminal
// (delivered) delivery is purgeable and the purge is idempotent; a
// pre-T03 legacy row with no delivery-core tracking is conservatively
// treated as not-done by default; PurgeMessageBody never mutates on a
// refusal.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	messaging "github.com/hollis-labs/substrate/mesh/messaging"
	"github.com/hollis-labs/substrate/mesh/messaging/delivery"

	"github.com/hollis-labs/tether/internal/store"
)

func openRetentionDB(t *testing.T) *store.Store {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "retention.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func sendRetentionMessage(t *testing.T, db *store.Store) string {
	t.Helper()
	sent, err := db.MessagingStore().Send(context.Background(), messaging.Envelope{
		Kind:    messaging.MsgKindNotice,
		From:    addr("sender"),
		To:      addr("worker"),
		Payload: []byte(`{"body":"hello"}`),
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	return sent.ID
}

func TestPurgeMessageBody_PendingDelivery_RefusesAndDoesNotMutate(t *testing.T) {
	db := openRetentionDB(t)
	id := sendRetentionMessage(t, db)

	purged, err := db.PurgeMessageBody(context.Background(), id, "msg://agent/test/operator")
	if !errors.Is(err, store.ErrPendingObligation) {
		t.Fatalf("err = %v, want ErrPendingObligation", err)
	}
	if purged {
		t.Fatalf("purged = true on a refusal, want false")
	}

	env, err := db.MessagingStore().Get(context.Background(), id)
	if err != nil {
		t.Fatalf("get after refused purge: %v", err)
	}
	if string(env.Payload) != `{"body":"hello"}` {
		t.Fatalf("payload after refused purge = %q, want unchanged", env.Payload)
	}
}

func TestPurgeMessageBody_LeasedDelivery_Refuses(t *testing.T) {
	db := openRetentionDB(t)
	id := sendRetentionMessage(t, db)
	deliveryID, ok, err := db.DeliveryIDForMessage(context.Background(), id)
	if err != nil || !ok {
		t.Fatalf("delivery id: ok=%v err=%v", ok, err)
	}
	if _, err := db.DeliveryStore().Claim(context.Background(), delivery.ClaimRequest{
		DeliveryID: delivery.DeliveryID(deliveryID), Holder: "h1", LeaseDuration: 30 * time.Second, Nowait: true,
	}); err != nil {
		t.Fatalf("claim: %v", err)
	}

	if _, err := db.PurgeMessageBody(context.Background(), id, "msg://agent/test/operator"); !errors.Is(err, store.ErrPendingObligation) {
		t.Fatalf("err = %v, want ErrPendingObligation for a leased (in-flight) delivery", err)
	}
}

func TestPurgeMessageBody_DeadLettered_RefusesBecauseStillRepairableViaRedrive(t *testing.T) {
	db := openRetentionDB(t)
	id := sendRetentionMessage(t, db)
	deliveryID, ok, err := db.DeliveryIDForMessage(context.Background(), id)
	if err != nil || !ok {
		t.Fatalf("delivery id: ok=%v err=%v", ok, err)
	}
	ds := db.DeliveryStore()
	claim, err := ds.Claim(context.Background(), delivery.ClaimRequest{
		DeliveryID: delivery.DeliveryID(deliveryID), Holder: "h1", LeaseDuration: 30 * time.Second, Nowait: true,
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	lease := delivery.LeaseRef{DeliveryID: claim.Attempt.DeliveryID, AttemptID: claim.Attempt.ID, LeaseToken: claim.Attempt.LeaseToken}
	if _, _, err := ds.Nack(context.Background(), delivery.NackRequest{Lease: lease, Retryable: false, Error: "boom"}); err != nil && !errors.Is(err, delivery.ErrDeadLettered) {
		t.Fatalf("nack: %v", err)
	}

	rd, err := ds.GetDelivery(context.Background(), delivery.DeliveryID(deliveryID))
	if err != nil || rd.Status != delivery.DeliveryDeadLettered {
		t.Fatalf("precondition: delivery status = %v, err=%v, want dead_lettered", rd.Status, err)
	}

	if _, err := db.PurgeMessageBody(context.Background(), id, "msg://agent/test/operator"); !errors.Is(err, store.ErrPendingObligation) {
		t.Fatalf("err = %v, want ErrPendingObligation -- a dead-lettered delivery remains redrivable, "+
			"purging its body first would make a later redrive resend an empty message", err)
	}
}

func TestPurgeMessageBody_Delivered_PurgesBodyAndIsIdempotent(t *testing.T) {
	db := openRetentionDB(t)
	id := sendRetentionMessage(t, db)
	deliveryID, ok, err := db.DeliveryIDForMessage(context.Background(), id)
	if err != nil || !ok {
		t.Fatalf("delivery id: ok=%v err=%v", ok, err)
	}
	ds := db.DeliveryStore()
	claim, err := ds.Claim(context.Background(), delivery.ClaimRequest{
		DeliveryID: delivery.DeliveryID(deliveryID), Holder: "h1", LeaseDuration: 30 * time.Second, Nowait: true,
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	lease := delivery.LeaseRef{DeliveryID: claim.Attempt.DeliveryID, AttemptID: claim.Attempt.ID, LeaseToken: claim.Attempt.LeaseToken}
	if _, _, err := ds.Ack(context.Background(), delivery.AckRequest{Lease: lease, Stage: delivery.StageConsumed}); err != nil {
		t.Fatalf("ack: %v", err)
	}

	purged, err := db.PurgeMessageBody(context.Background(), id, "msg://agent/test/operator")
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if !purged {
		t.Fatalf("purged = false on the first purge of a delivered message, want true")
	}

	env, err := db.MessagingStore().Get(context.Background(), id)
	if err != nil {
		t.Fatalf("get after purge: %v", err)
	}
	if env.Payload != nil {
		t.Errorf("payload after purge = %q, want cleared", env.Payload)
	}
	if env.From.URN() != addr("sender").URN() || env.To.URN() != addr("worker").URN() {
		t.Errorf("structural fields not preserved after purge: from=%s to=%s", env.From.URN(), env.To.URN())
	}

	purgedAgain, err := db.PurgeMessageBody(context.Background(), id, "msg://agent/test/operator")
	if err != nil {
		t.Fatalf("second purge: %v", err)
	}
	if purgedAgain {
		t.Errorf("purged = true on the second (already-purged) call, want false (idempotent no-op)")
	}
}

func TestPurgeMessageBody_NotFound(t *testing.T) {
	db := openRetentionDB(t)
	if _, err := db.PurgeMessageBody(context.Background(), "no-such-message", "msg://agent/test/operator"); !errors.Is(err, messaging.ErrNotFound) {
		t.Fatalf("err = %v, want wrapped messaging.ErrNotFound", err)
	}
}

// insertLegacyMessage writes a pre-T03 message row directly (no
// delivery_id — mirrors internal/api/trace_test.go's legacy-row fixture),
// optionally marking it consumed via the old delivered_at/consumed_at
// state machine that T03 superseded.
func insertLegacyMessage(t *testing.T, db *store.Store, id string, consumed bool) {
	t.Helper()
	var consumedAt any
	if consumed {
		consumedAt = time.Now().UTC().Format(time.RFC3339)
	}
	if _, err := db.DB().Exec(
		`INSERT INTO messages (id, kind, channel, from_urn, to_urn, payload, created_at, consumed_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id, "notice", "", addr("sender").URN(), addr("worker").URN(), `{"body":"legacy"}`,
		time.Now().UTC().Format(time.RFC3339), consumedAt,
	); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}
}

func TestPurgeMessageBody_LegacyRowNoDeliveryTracking_ConservativeByDefault(t *testing.T) {
	db := openRetentionDB(t)
	insertLegacyMessage(t, db, "legacy-pending", false)

	if _, err := db.PurgeMessageBody(context.Background(), "legacy-pending", "msg://agent/test/operator"); !errors.Is(err, store.ErrPendingObligation) {
		t.Fatalf("err = %v, want ErrPendingObligation -- a legacy row with no completion signal "+
			"and no delivery-core tracking must default to not-eligible, never silently purge", err)
	}
}

// insertGroupCanonicalRow mimics the one column shape a group message's
// canonical `messages` row actually has after fanout (registry/group_fanout.go:
// delivery_message_id set, delivery_id always NULL -- migration 0021's
// comment names this distinction explicitly) with no consumed_at/canceled_at,
// since group reads are non-destructive (last_read_seq-driven, per
// GroupMessage's doc comment) and never set either column.
func insertGroupCanonicalRow(t *testing.T, db *store.Store, id string, ageHours int) {
	t.Helper()
	if _, err := db.DB().Exec(
		`INSERT INTO messages (id, kind, channel, from_urn, to_urn, payload, created_at, delivery_message_id)
		 VALUES (?, ?, ?, ?, ?, ?, datetime('now', ?), ?)`,
		id, "notice", "", addr("owner").URN(), addr("group").URN(), `{"text":"hi all"}`,
		fmt.Sprintf("-%d hours", ageHours), "dmsg-fanout-fixture",
	); err != nil {
		t.Fatalf("insert group canonical row: %v", err)
	}
}

// TestPurgeMessageBody_GroupCanonicalRow_NeverEligible locks in the
// documented (retention.go file comment) scope boundary: a group
// message's canonical row can never become purge-eligible, regardless of
// age, because this schema has no per-member completion signal for it --
// only delivery_id (1:1-only) is ever inspected, and group rows only ever
// carry delivery_message_id. This must stay refusing forever, not become
// eligible once naively "old enough."
func TestPurgeMessageBody_GroupCanonicalRow_NeverEligible(t *testing.T) {
	db := openRetentionDB(t)
	insertGroupCanonicalRow(t, db, "group-canonical-old", 24*365)

	if _, err := db.PurgeMessageBody(context.Background(), "group-canonical-old", "msg://agent/test/operator"); !errors.Is(err, store.ErrPendingObligation) {
		t.Fatalf("err = %v, want ErrPendingObligation -- a group canonical row must never be purge-eligible "+
			"under this schema, no matter how old", err)
	}

	candidates, err := db.ListRetentionCandidates(context.Background(), time.Now().UTC().Add(-time.Hour))
	if err != nil {
		t.Fatalf("list candidates: %v", err)
	}
	for _, c := range candidates {
		if c.MessageID == "group-canonical-old" && c.Eligible {
			t.Fatalf("candidate %+v Eligible=true, want false for a group canonical row", c)
		}
	}
}

func TestPurgeMessageBody_LegacyRowMarkedConsumed_IsEligible(t *testing.T) {
	db := openRetentionDB(t)
	insertLegacyMessage(t, db, "legacy-consumed", true)

	purged, err := db.PurgeMessageBody(context.Background(), "legacy-consumed", "msg://agent/test/operator")
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if !purged {
		t.Fatalf("purged = false for a legacy row whose own consumed_at marks it done")
	}
}

func TestListRetentionCandidates_AnnotatesEligibilityAndRespectsCutoff(t *testing.T) {
	db := openRetentionDB(t)
	ctx := context.Background()

	pendingID := sendRetentionMessage(t, db)

	deliveredID := sendRetentionMessage(t, db)
	dDeliveryID, _, _ := db.DeliveryIDForMessage(ctx, deliveredID)
	ds := db.DeliveryStore()
	claim, err := ds.Claim(ctx, delivery.ClaimRequest{DeliveryID: delivery.DeliveryID(dDeliveryID), Holder: "h1", LeaseDuration: 30 * time.Second, Nowait: true})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	lease := delivery.LeaseRef{DeliveryID: claim.Attempt.DeliveryID, AttemptID: claim.Attempt.ID, LeaseToken: claim.Attempt.LeaseToken}
	if _, _, err := ds.Ack(ctx, delivery.AckRequest{Lease: lease, Stage: delivery.StageConsumed}); err != nil {
		t.Fatalf("ack: %v", err)
	}

	insertLegacyMessage(t, db, "legacy-old", true)

	// A message created "in the future" relative to the cutoff must be
	// excluded from the candidate list entirely, regardless of eligibility.
	tooRecentID := "too-recent"
	if _, err := db.DB().Exec(
		`INSERT INTO messages (id, kind, channel, from_urn, to_urn, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		tooRecentID, "notice", "", addr("sender").URN(), addr("worker").URN(),
		time.Now().UTC().Add(time.Hour).Format(time.RFC3339),
	); err != nil {
		t.Fatalf("insert too-recent row: %v", err)
	}

	candidates, err := db.ListRetentionCandidates(ctx, time.Now().UTC().Add(time.Minute))
	if err != nil {
		t.Fatalf("list candidates: %v", err)
	}

	byID := map[string]store.RetentionCandidate{}
	for _, c := range candidates {
		byID[c.MessageID] = c
	}

	if _, present := byID[tooRecentID]; present {
		t.Errorf("candidate list includes %s, which is newer than the cutoff", tooRecentID)
	}

	pending, ok := byID[pendingID]
	if !ok {
		t.Fatalf("candidate list missing pending message %s", pendingID)
	}
	if !pending.HasDelivery || pending.Eligible {
		t.Errorf("pending candidate = %+v, want HasDelivery=true Eligible=false", pending)
	}

	delivered, ok := byID[deliveredID]
	if !ok {
		t.Fatalf("candidate list missing delivered message %s", deliveredID)
	}
	if !delivered.HasDelivery || !delivered.Eligible || delivered.Status != string(delivery.DeliveryDelivered) {
		t.Errorf("delivered candidate = %+v, want HasDelivery=true Eligible=true Status=delivered", delivered)
	}

	legacy, ok := byID["legacy-old"]
	if !ok {
		t.Fatalf("candidate list missing legacy message")
	}
	if legacy.HasDelivery || !legacy.Eligible {
		t.Errorf("legacy candidate = %+v, want HasDelivery=false Eligible=true (consumed_at set)", legacy)
	}
}

func TestPurgeMessageBody_AuditSurvivesReopenAndMessageDeletion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	insertLegacyMessage(t, db, "durable", true)
	author := "msg://agent/test/operator"
	before := time.Now().UTC()
	if purged, err := db.PurgeMessageBody(context.Background(), "durable", author); err != nil || !purged {
		t.Fatalf("purge: %v, %v", purged, err)
	}
	if _, err := db.DB().Exec(`DELETE FROM messages WHERE id='durable'`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var table, id, by, at string
	if err := reopened.DB().QueryRow(`SELECT table_name,message_id,authorized_by,at FROM message_purge_audit`).Scan(&table, &id, &by, &at); err != nil {
		t.Fatal(err)
	}
	stamp, err := time.Parse(time.RFC3339Nano, at)
	if err != nil || stamp.Before(before) || stamp.After(time.Now()) {
		t.Fatalf("audit time: %q, %v", at, err)
	}
	if table != "messages" || id != "durable" || by != author {
		t.Fatalf("audit: %s %s %s", table, id, by)
	}
}

func TestPurgeMessageBody_AuditFailureRollsBackPayloadAndMetadata(t *testing.T) {
	db := openRetentionDB(t)
	insertLegacyMessage(t, db, "rollback", true)
	if _, err := db.DB().Exec(`UPDATE messages SET metadata='{"private":"meta"}' WHERE id='rollback'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().Exec(`CREATE TRIGGER reject_message_purge_audit BEFORE INSERT ON message_purge_audit BEGIN SELECT RAISE(ABORT,'audit unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if purged, err := db.PurgeMessageBody(context.Background(), "rollback", "msg://agent/test/operator"); err == nil || purged {
		t.Fatalf("failed audit: %v, %v", purged, err)
	}
	var payload, metadata sql.NullString
	if err := db.DB().QueryRow(`SELECT payload,metadata FROM messages WHERE id='rollback'`).Scan(&payload, &metadata); err != nil {
		t.Fatal(err)
	}
	if payload.String != `{"body":"legacy"}` || metadata.String != `{"private":"meta"}` {
		t.Fatalf("failed audit lost content: %v %v", payload, metadata)
	}
}

func TestPurgeMessageBody_ConcurrentRetriesHaveOneReceipt(t *testing.T) {
	db := openRetentionDB(t)
	insertLegacyMessage(t, db, "retry", true)
	var wg sync.WaitGroup
	results := make(chan bool, 8)
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			purged, err := db.PurgeMessageBody(context.Background(), "retry", "msg://agent/test/operator")
			results <- purged
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	changed := 0
	for result := range results {
		if result {
			changed++
		}
	}
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := db.DB().QueryRow(`SELECT COUNT(*) FROM message_purge_audit WHERE message_id='retry'`).Scan(&count); err != nil || changed != 1 || count != 1 {
		t.Fatalf("retry changed=%d receipts=%d err=%v", changed, count, err)
	}
}

func TestPurgeMessageBody_RequiresAuthorInStore(t *testing.T) {
	db := openRetentionDB(t)
	insertLegacyMessage(t, db, "author", true)
	for _, author := range []string{"", "not-a-urn"} {
		if purged, err := db.PurgeMessageBody(context.Background(), "author", author); err == nil || purged {
			t.Fatalf("accepted author %q: %v %v", author, purged, err)
		}
	}
}
