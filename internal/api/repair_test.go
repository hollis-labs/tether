package api

// repair_test.go — end-to-end coverage for POST /messages/{id}/redrive
// (T09, messaging vNext). Wires a real *store.Store (real SQLite
// delivery core) so idempotency and the group-fanout frozen-recipient
// property are exercised genuinely, not mocked.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	messaging "github.com/hollis-labs/go-messaging"
	"github.com/hollis-labs/go-messaging/delivery"

	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/store"
)

// mustParseURN is a small test helper so fixtures can go from a
// registry.Profile.URN string to a messaging.Address without hand-
// building one field by field.
func mustParseURN(t *testing.T, urn string) messaging.Address {
	t.Helper()
	addr, err := messaging.ParseURN(urn)
	if err != nil {
		t.Fatalf("parse URN %q: %v", urn, err)
	}
	return addr
}

func postRedrive(t *testing.T, base, messageID string, body map[string]any) (*http.Response, messageRedriveResponse) {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	resp, err := http.Post(base+"/messages/"+messageID+"/redrive", "application/json", reader)
	if err != nil {
		t.Fatalf("POST redrive: %v", err)
	}
	defer resp.Body.Close()
	var out messageRedriveResponse
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	return resp, out
}

func newRepairTestServer(t *testing.T) (string, *store.Store) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "repair.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	srv := httptest.NewServer(NewHandler(Deps{MessageStore: db.MessagingStore(), DeliveryRepair: db}))
	t.Cleanup(srv.Close)
	return srv.URL, db
}

func TestMessageRedrive_DeadLetteredDelivery_HappyPath(t *testing.T) {
	base, db := newRepairTestServer(t)
	ctx := context.Background()
	from := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "sender"}
	to := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "worker"}
	sent, err := db.MessagingStore().Send(ctx, messaging.Envelope{Kind: messaging.MsgKindNotice, From: from, To: to})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	deadLetterDelivery(t, db, sent.ID, "s1")

	resp, out := postRedrive(t, base, sent.ID, map[string]any{"authorized_by": "msg://agent/test/operator"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !out.Redriven || out.Status != string(delivery.DeliveryPending) {
		t.Fatalf("response = %+v, want redriven=true status=pending", out)
	}
}

func TestMessageRedrive_Idempotent_SecondCallIsNoOpNotError(t *testing.T) {
	base, db := newRepairTestServer(t)
	ctx := context.Background()
	from := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "sender"}
	to := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "worker"}
	sent, err := db.MessagingStore().Send(ctx, messaging.Envelope{Kind: messaging.MsgKindNotice, From: from, To: to})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	deadLetterDelivery(t, db, sent.ID, "s1")

	body := map[string]any{"authorized_by": "msg://agent/test/operator"}
	first, out1 := postRedrive(t, base, sent.ID, body)
	if first.StatusCode != http.StatusOK || !out1.Redriven {
		t.Fatalf("first redrive: status=%d out=%+v, want 200/redriven=true", first.StatusCode, out1)
	}

	second, out2 := postRedrive(t, base, sent.ID, body)
	if second.StatusCode != http.StatusOK {
		t.Fatalf("second redrive: status=%d, want 200 (idempotent, not an error)", second.StatusCode)
	}
	if out2.Redriven {
		t.Fatalf("second redrive: %+v, want redriven=false (already retryable)", out2)
	}
}

func TestMessageRedrive_TerminalDelivered_Conflict(t *testing.T) {
	base, db := newRepairTestServer(t)
	ctx := context.Background()
	from := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "sender"}
	to := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "worker"}
	sent, err := db.MessagingStore().Send(ctx, messaging.Envelope{Kind: messaging.MsgKindNotice, From: from, To: to})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if err := db.MessagingStore().Consume(ctx, sent.ID, to); err != nil {
		t.Fatalf("consume: %v", err)
	}

	resp, _ := postRedrive(t, base, sent.ID, map[string]any{"authorized_by": "msg://agent/test/operator"})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (delivered, redrive does not apply)", resp.StatusCode)
	}
}

func TestMessageRedrive_RequiresAuthorizedBy(t *testing.T) {
	base, db := newRepairTestServer(t)
	ctx := context.Background()
	from := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "sender"}
	to := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "worker"}
	sent, err := db.MessagingStore().Send(ctx, messaging.Envelope{Kind: messaging.MsgKindNotice, From: from, To: to})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	resp, _ := postRedrive(t, base, sent.ID, map[string]any{})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestMessageRedrive_NotConfigured_404(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "no-repair.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	srv := httptest.NewServer(NewHandler(Deps{MessageStore: db.MessagingStore()}))
	t.Cleanup(srv.Close)

	ctx := context.Background()
	sent, err := db.MessagingStore().Send(ctx, messaging.Envelope{
		Kind: messaging.MsgKindNotice,
		From: messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "s"},
		To:   messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "w"},
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	resp, _ := postRedrive(t, srv.URL, sent.ID, map[string]any{"authorized_by": "msg://agent/test/operator"})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (DeliveryRepair not configured)", resp.StatusCode)
	}
}

// TestMessageRedrive_GroupFanout_TargetsFrozenRecipient is acceptance
// #2's core proof: redriving a group-fanout delivery must retry the SAME
// member the delivery was originally frozen for at fanout time, even
// after that member left and a different member joined -- never the new
// member. This is the concrete "cannot... retry frozen fanout against
// new actors" test.
func TestMessageRedrive_GroupFanout_TargetsFrozenRecipient(t *testing.T) {
	base, db := newRepairTestServer(t)
	ctx := context.Background()
	reg := registry.NewService(registry.NewStorage(db.DB()))
	reg.SetDeliveryStore(db.DeliveryStore())

	owner, err := reg.Register(ctx, registry.KindAgent, registry.Profile{DisplayName: "Owner", LastUpdatedBy: "tester"})
	if err != nil {
		t.Fatalf("register owner: %v", err)
	}
	bob, err := reg.Register(ctx, registry.KindAgent, registry.Profile{DisplayName: "Bob", LastUpdatedBy: "tester"})
	if err != nil {
		t.Fatalf("register bob: %v", err)
	}
	group, err := reg.Register(ctx, registry.KindGroup, registry.Profile{DisplayName: "Room", LastUpdatedBy: owner.URN})
	if err != nil {
		t.Fatalf("register group: %v", err)
	}
	if _, err := reg.AddMember(ctx, group.URN, bob.URN, owner.URN, registry.MemberRoleMember); err != nil {
		t.Fatalf("add bob: %v", err)
	}

	gm, err := reg.SendToGroup(ctx, group.URN, owner.URN, "notice", "", "application/json", json.RawMessage(`{"text":"hi"}`))
	if err != nil {
		t.Fatalf("send to group: %v", err)
	}

	// Find bob's frozen fanout delivery and dead-letter it.
	deliveries, err := db.DeliveryStore().ListDeliveries(ctx, delivery.Filter{Recipient: mustParseURN(t, bob.URN)})
	if err != nil {
		t.Fatalf("list deliveries: %v", err)
	}
	if len(deliveries) != 1 {
		t.Fatalf("expected exactly 1 delivery for bob, got %d: %+v", len(deliveries), deliveries)
	}
	bobDeliveryID := deliveries[0].ID
	deadLetterDeliveryByID(t, db, bobDeliveryID, "s-bob")

	// Bob leaves; Carol joins AFTER the original fanout and dead-letter.
	if err := reg.RemoveMember(ctx, group.URN, bob.URN, owner.URN); err != nil {
		t.Fatalf("remove bob: %v", err)
	}
	carol, err := reg.Register(ctx, registry.KindAgent, registry.Profile{DisplayName: "Carol", LastUpdatedBy: "tester"})
	if err != nil {
		t.Fatalf("register carol: %v", err)
	}
	if _, err := reg.AddMember(ctx, group.URN, carol.URN, owner.URN, registry.MemberRoleMember); err != nil {
		t.Fatalf("add carol: %v", err)
	}

	// Redrive bob's specific fanout delivery by its OWN delivery id --
	// group-fanout deliveries have no row in Tether's messages table at
	// all (see repair.go's file doc), so this is the only unambiguous
	// way to address exactly bob's obligation and not, say, a
	// hypothetical other member's.
	resp, out := postRedrive(t, base, string(bobDeliveryID), map[string]any{"authorized_by": owner.URN})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("redrive status = %d, want 200", resp.StatusCode)
	}
	if !out.Redriven {
		t.Fatalf("redrive response = %+v, want redriven=true", out)
	}

	// The redriven delivery must still be addressed to bob -- never carol.
	after, err := db.DeliveryStore().GetDelivery(ctx, bobDeliveryID)
	if err != nil {
		t.Fatalf("get delivery after redrive: %v", err)
	}
	if after.Recipient.URN() != bob.URN {
		t.Fatalf("redriven delivery recipient = %q, want bob (%q) -- frozen fanout must never retarget a new actor", after.Recipient.URN(), bob.URN)
	}

	// Carol must have received NOTHING from this redrive -- no new
	// delivery was created for her.
	carolDeliveries, err := db.DeliveryStore().ListDeliveries(ctx, delivery.Filter{Recipient: mustParseURN(t, carol.URN)})
	if err != nil {
		t.Fatalf("list carol deliveries: %v", err)
	}
	if len(carolDeliveries) != 0 {
		t.Fatalf("carol has %d deliveries, want 0 -- redrive must not fan out to a member who joined after the original send", len(carolDeliveries))
	}
	if gm.ID == "" {
		t.Fatal("sanity: expected SendToGroup to return a non-empty canonical room message id")
	}
}

// deadLetterDelivery claims and non-retryably nacks the delivery for
// messageID, dead-lettering it -- the fixture setup TestMessageRedrive_*
// needs for a redrive to have something real to act on.
func deadLetterDelivery(t *testing.T, db *store.Store, messageID, holder string) {
	t.Helper()
	deliveryID, ok, err := db.DeliveryIDForMessage(context.Background(), messageID)
	if err != nil || !ok {
		t.Fatalf("delivery id for %s: ok=%v err=%v", messageID, ok, err)
	}
	deadLetterDeliveryByID(t, db, delivery.DeliveryID(deliveryID), holder)
}

func deadLetterDeliveryByID(t *testing.T, db *store.Store, deliveryID delivery.DeliveryID, holder string) {
	t.Helper()
	ctx := context.Background()
	ds := db.DeliveryStore()
	claim, err := ds.Claim(ctx, delivery.ClaimRequest{DeliveryID: deliveryID, Holder: holder, LeaseDuration: 30 * time.Second, Nowait: true})
	if err != nil {
		t.Fatalf("claim %s: %v", deliveryID, err)
	}
	lease := delivery.LeaseRef{DeliveryID: claim.Attempt.DeliveryID, AttemptID: claim.Attempt.ID, LeaseToken: claim.Attempt.LeaseToken}
	_, _, err = ds.Nack(ctx, delivery.NackRequest{Lease: lease, Retryable: false, Error: "test dead-letter"})
	if err != nil && !errors.Is(err, delivery.ErrDeadLettered) {
		t.Fatalf("nack (dead-letter) %s: %v", deliveryID, err)
	}
}
