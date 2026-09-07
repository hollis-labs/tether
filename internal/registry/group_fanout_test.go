package registry_test

// group_fanout_test.go — T04 acceptance evidence for durable group fanout.
// Uses delivery.NewMemoryStore() (go-messaging's in-memory reference Store)
// as the GroupFanoutDeliveryStore -- it satisfies the narrow Enqueue-only
// seam group_fanout.go depends on, keeping these tests independent of
// internal/store's SQLite wiring (that integration is covered separately
// by internal/store's own delivery_store_test.go, which exercises the real
// SQLite-backed path end to end).

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/hollis-labs/go-messaging/delivery"

	"github.com/hollis-labs/tether/internal/registry"
)

func TestGroupFanout_CreatesPerRecipientObligationsExcludingSender(t *testing.T) {
	svc, storage := newServiceWithStorage(t)
	ds := delivery.NewMemoryStore()
	svc.SetDeliveryStore(ds)
	ctx := context.Background()

	g, owner := groupFixture(t, svc, "Fanout-Basic")
	bob := registerCreator(t, svc, "Bob")
	carol := registerCreator(t, svc, "Carol")
	if _, err := svc.AddMember(ctx, g.URN, bob.URN, owner.URN, registry.MemberRoleMember); err != nil {
		t.Fatalf("add bob: %v", err)
	}
	if _, err := svc.AddMember(ctx, g.URN, carol.URN, owner.URN, registry.MemberRoleMember); err != nil {
		t.Fatalf("add carol: %v", err)
	}

	gm, err := svc.SendToGroup(ctx, g.URN, owner.URN, "notice", "", "application/json", json.RawMessage(`{"text":"hi all"}`))
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	// Room body: exactly one canonical row, unaffected by fanout.
	msgs, err := svc.ListGroupMessages(ctx, g.URN, owner.URN, 0, "", 10)
	if err != nil {
		t.Fatalf("list group messages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected exactly one canonical room message, got %d", len(msgs))
	}

	deliveryMsgID, ok := lookupDeliveryMessageID(t, storage, gm.ID)
	if !ok {
		t.Fatalf("expected a delivery_message_id mapping to be recorded for the group post")
	}
	deliveries, err := ds.ListDeliveries(ctx, delivery.Filter{MessageID: delivery.MessageID(deliveryMsgID)})
	if err != nil {
		t.Fatalf("list deliveries: %v", err)
	}
	if len(deliveries) != 2 {
		t.Fatalf("expected exactly 2 fanout deliveries (bob + carol, sender excluded), got %d", len(deliveries))
	}
	recipients := map[string]bool{}
	for _, d := range deliveries {
		recipients[d.Recipient.URN()] = true
	}
	if !recipients[bob.URN] || !recipients[carol.URN] {
		t.Fatalf("expected bob and carol as recipients, got %+v", recipients)
	}
	if recipients[owner.URN] {
		t.Fatalf("sender must not receive a delivery obligation for their own post")
	}
}

func TestGroupFanout_FrozenRecipientSetSurvivesLaterMembershipChange(t *testing.T) {
	svc, storage := newServiceWithStorage(t)
	ds := delivery.NewMemoryStore()
	svc.SetDeliveryStore(ds)
	ctx := context.Background()

	g, owner := groupFixture(t, svc, "Fanout-Frozen")
	bob := registerCreator(t, svc, "Bob")
	if _, err := svc.AddMember(ctx, g.URN, bob.URN, owner.URN, registry.MemberRoleMember); err != nil {
		t.Fatalf("add bob: %v", err)
	}

	gm, err := svc.SendToGroup(ctx, g.URN, owner.URN, "notice", "", "", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	deliveryMsgID, _ := lookupDeliveryMessageID(t, storage, gm.ID)

	// Bob leaves AFTER the send. The already-created delivery obligation
	// must be untouched -- retries can't re-resolve to a replacement, and
	// removing bob can't retroactively erase an obligation already frozen.
	if err := svc.RemoveMember(ctx, g.URN, bob.URN, owner.URN); err != nil {
		t.Fatalf("remove bob: %v", err)
	}
	carol := registerCreator(t, svc, "Carol")
	if _, err := svc.AddMember(ctx, g.URN, carol.URN, owner.URN, registry.MemberRoleMember); err != nil {
		t.Fatalf("add carol: %v", err)
	}

	deliveries, err := ds.ListDeliveries(ctx, delivery.Filter{MessageID: delivery.MessageID(deliveryMsgID)})
	if err != nil {
		t.Fatalf("list deliveries: %v", err)
	}
	if len(deliveries) != 1 || deliveries[0].Recipient.URN() != bob.URN {
		t.Fatalf("expected the frozen recipient set to still be exactly [bob], got %+v", deliveries)
	}
}

func TestGroupFanout_RoomBodyAndReadCursorUnaffectedByFanout(t *testing.T) {
	svc, _ := newServiceWithStorage(t)
	ds := delivery.NewMemoryStore()
	svc.SetDeliveryStore(ds)
	ctx := context.Background()

	g, owner := groupFixture(t, svc, "Fanout-ReadCursor")
	bob := registerCreator(t, svc, "Bob")
	if _, err := svc.AddMember(ctx, g.URN, bob.URN, owner.URN, registry.MemberRoleMember); err != nil {
		t.Fatalf("add bob: %v", err)
	}

	for i := 0; i < 3; i++ {
		if _, err := svc.SendToGroup(ctx, g.URN, owner.URN, "notice", "", "", json.RawMessage(`{}`)); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}

	// Bob's read cursor is independent of the new delivery-core receipts --
	// nothing in the fanout path touches group_members.last_read_seq.
	if err := svc.MarkRead(ctx, g.URN, bob.URN, 2); err != nil {
		t.Fatalf("mark read: %v", err)
	}
	unread, err := svc.ListGroupMessages(ctx, g.URN, bob.URN, 2, "", 10)
	if err != nil {
		t.Fatalf("list unread: %v", err)
	}
	if len(unread) != 1 || unread[0].GroupSeq != 3 {
		t.Fatalf("expected exactly one unread message (seq 3) independent of fanout receipts, got %+v", unread)
	}

	// The room body remains exactly 3 rows -- fanout never duplicates content.
	all, err := svc.ListGroupMessages(ctx, g.URN, owner.URN, 0, "", 10)
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("expected exactly 3 canonical room messages (no duplication from fanout), got %d", len(all))
	}
}

func TestGroupFanout_ConcurrentReadCursorUpdatesNeverRegress(t *testing.T) {
	svc := newServiceForConcurrency(t)
	ctx := context.Background()
	g, owner := groupFixture(t, svc, "Fanout-ConcurrentCursor")
	bob := registerCreator(t, svc, "Bob")
	if _, err := svc.AddMember(ctx, g.URN, bob.URN, owner.URN, registry.MemberRoleMember); err != nil {
		t.Fatalf("add bob: %v", err)
	}
	for i := 0; i < 10; i++ {
		if _, err := svc.SendToGroup(ctx, g.URN, owner.URN, "notice", "", "", json.RawMessage(`{}`)); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}

	const n = 10
	var wg sync.WaitGroup
	wg.Add(n)
	errs := make([]error, n)
	for i := 1; i <= n; i++ {
		go func(seq int64) {
			defer wg.Done()
			errs[seq-1] = svc.MarkRead(ctx, g.URN, bob.URN, seq)
		}(int64(i))
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("markread %d: %v", i+1, err)
		}
	}

	// Monotonic max regardless of goroutine completion order: cursor must
	// land at 10, never regress to a smaller concurrently-issued value.
	unread, err := svc.ListGroupMessages(ctx, g.URN, bob.URN, 0, "", 100)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(unread) != 0 {
		t.Fatalf("expected the cursor to have advanced to the highest concurrently-marked seq (10), leaving 0 unread, got %d unread", len(unread))
	}
}

func TestGroupFanout_DisabledWhenNoDeliveryStoreConfigured(t *testing.T) {
	// No SetDeliveryStore call at all -- SendToGroup must still succeed
	// (fanout is purely additive; its absence is not a send failure).
	svc, storage := newServiceWithStorage(t)
	ctx := context.Background()
	g, owner := groupFixture(t, svc, "Fanout-Disabled")

	gm, err := svc.SendToGroup(ctx, g.URN, owner.URN, "notice", "", "", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("send without a delivery store configured must still succeed: %v", err)
	}
	if _, ok := lookupDeliveryMessageID(t, storage, gm.ID); ok {
		t.Fatalf("expected no delivery_message_id mapping when fanout is disabled")
	}
}

// failingDeliveryStore always errors on Enqueue, simulating a genuinely
// unreachable/broken delivery core (as opposed to
// TestGroupFanout_DisabledWhenNoDeliveryStoreConfigured's "not configured
// at all" case).
type failingDeliveryStore struct{}

func (failingDeliveryStore) Enqueue(context.Context, delivery.EnqueueRequest) (delivery.EnqueueResult, error) {
	return delivery.EnqueueResult{}, errors.New("simulated delivery core outage")
}

// TestGroupFanout_EnqueueFailureIsSurfacedNotSwallowed is the regression
// test for the gap found during this task's T12 handoff review: a fully
// failed Enqueue call used to be indistinguishable from success to the
// sender (SendToGroup always returned a nil error), leaving the post with
// ZERO recipient delivery obligations and no signal anything was wrong.
// The room post must still succeed (fanout failure never rolls it back),
// but the caller must now be told via GroupMessage.FanoutError.
func TestGroupFanout_EnqueueFailureIsSurfacedNotSwallowed(t *testing.T) {
	svc, storage := newServiceWithStorage(t)
	svc.SetDeliveryStore(failingDeliveryStore{})
	ctx := context.Background()

	g, owner := groupFixture(t, svc, "Fanout-Enqueue-Failure")
	bob := registerCreator(t, svc, "Bob")
	if _, err := svc.AddMember(ctx, g.URN, bob.URN, owner.URN, registry.MemberRoleMember); err != nil {
		t.Fatalf("add bob: %v", err)
	}

	gm, err := svc.SendToGroup(ctx, g.URN, owner.URN, "notice", "", "application/json", json.RawMessage(`{"text":"hi"}`))
	if err != nil {
		t.Fatalf("send must still succeed despite the fanout failure: %v", err)
	}
	if gm.FanoutError == "" {
		t.Fatalf("expected GroupMessage.FanoutError to be set when Enqueue fails -- the caller must not see unqualified success")
	}

	// Room body is still durable and unaffected.
	msgs, err := svc.ListGroupMessages(ctx, g.URN, owner.URN, 0, "", 10)
	if err != nil {
		t.Fatalf("list group messages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected the room post to survive the fanout failure, got %d messages", len(msgs))
	}
	// A re-read of the same message must not report the transient
	// send-time failure as if it were current/stored state.
	if msgs[0].FanoutError != "" {
		t.Fatalf("FanoutError must not be persisted/replayed on a later read, got %q", msgs[0].FanoutError)
	}

	if _, ok := lookupDeliveryMessageID(t, storage, gm.ID); ok {
		t.Fatalf("expected no delivery_message_id mapping when Enqueue itself failed")
	}
}

// lookupDeliveryMessageID reads messages.delivery_message_id directly --
// registry.Storage doesn't expose a getter (only the setter, used by
// group_fanout.go), so tests reach through registry.DBForTest.
func lookupDeliveryMessageID(t *testing.T, storage *registry.Storage, messageID string) (string, bool) {
	t.Helper()
	db := registry.DBForTest(storage)
	var v sql.NullString
	if err := db.QueryRow(`SELECT delivery_message_id FROM messages WHERE id=?`, messageID).Scan(&v); err != nil {
		t.Fatalf("lookup delivery_message_id: %v", err)
	}
	return v.String, v.Valid
}
