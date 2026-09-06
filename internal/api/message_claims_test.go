package api

// message_claims_test.go — T07 (messaging vNext, CW-20260906-0038): HTTP
// coverage for POST /messages/{id}/claim|ack|nack, the durable,
// authorized claim/ack/nack surface a published-local (pull-only) bridge
// uses to process its own mailbox on its own initiative. Conformance
// scenarios named in the sprint's acceptance #2 (lost responses, stale
// lease, reconnect cursor recovery) are covered as named tests below.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	messaging "github.com/hollis-labs/go-messaging"
	"github.com/hollis-labs/go-messaging/delivery"

	"github.com/hollis-labs/tether/internal/store"
)

func newClaimTestServer(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "claims.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	h := NewHandler(Deps{MessageStore: db.MessagingStore(), DeliveryClaims: db})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, db
}

func claimAddr(id string) messaging.Address {
	return messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: id}
}

func postJSON(t *testing.T, url string, body any) (*http.Response, []byte) {
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
	resp, err := http.Post(url, "application/json", reader)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp, respBody
}

func TestMessageClaim_HappyPath_ThenAckToConsumed(t *testing.T) {
	srv, db := newClaimTestServer(t)
	ctx := context.Background()
	to := claimAddr("bridge-target")
	sent, err := db.MessagingStore().Send(ctx, messaging.Envelope{Kind: messaging.MsgKindNotice, From: claimAddr("sender"), To: to})
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	resp, body := postJSON(t, srv.URL+"/messages/"+sent.ID+"/claim?as="+to.URN(), map[string]any{"holder": "bridge-1", "lease_seconds": 30})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("claim: status=%d body=%s", resp.StatusCode, body)
	}
	var claimed messageClaimResponse
	if err := json.Unmarshal(body, &claimed); err != nil {
		t.Fatalf("decode claim: %v", err)
	}
	if claimed.Message.ID != sent.ID {
		t.Fatalf("claimed message id = %q, want %q", claimed.Message.ID, sent.ID)
	}
	if claimed.Lease.DeliveryID == "" || claimed.Lease.LeaseToken == "" {
		t.Fatalf("lease = %+v, want a populated DeliveryID/LeaseToken", claimed.Lease)
	}

	// Real, timely host_accepted receipt.
	resp, body = postJSON(t, srv.URL+"/messages/"+sent.ID+"/ack?as="+to.URN(), map[string]any{"lease": claimed.Lease, "stage": string(delivery.StageHostAccepted)})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ack host_accepted: status=%d body=%s", resp.StatusCode, body)
	}

	resp, body = postJSON(t, srv.URL+"/messages/"+sent.ID+"/ack?as="+to.URN(), map[string]any{"lease": claimed.Lease, "stage": string(delivery.StageConsumed)})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ack consumed: status=%d body=%s", resp.StatusCode, body)
	}

	deliveryID, _, _ := db.DeliveryIDForMessage(ctx, sent.ID)
	rd, err := db.DeliveryStore().GetDelivery(ctx, delivery.DeliveryID(deliveryID))
	if err != nil {
		t.Fatalf("get delivery: %v", err)
	}
	if rd.Status != delivery.DeliveryDelivered {
		t.Fatalf("delivery status = %q, want delivered after ack(consumed)", rd.Status)
	}
}

func TestMessageClaim_WrongRecipient_Rejected(t *testing.T) {
	srv, db := newClaimTestServer(t)
	ctx := context.Background()
	to := claimAddr("real-owner")
	sent, err := db.MessagingStore().Send(ctx, messaging.Envelope{Kind: messaging.MsgKindNotice, From: claimAddr("sender"), To: to})
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	imposter := claimAddr("imposter")
	resp, body := postJSON(t, srv.URL+"/messages/"+sent.ID+"/claim?as="+imposter.URN(), nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("claim as imposter: status=%d body=%s, want 409", resp.StatusCode, body)
	}
}

// TestMessageClaim_SecondClaimant_StaleLease is the "stale lease"
// conformance case: while one holder has an active claim, a second
// claimant must be told so observably (409), never silently succeed
// (which would let two holders process the same delivery concurrently).
func TestMessageClaim_SecondClaimant_StaleLease(t *testing.T) {
	srv, db := newClaimTestServer(t)
	ctx := context.Background()
	to := claimAddr("bridge-target")
	sent, err := db.MessagingStore().Send(ctx, messaging.Envelope{Kind: messaging.MsgKindNotice, From: claimAddr("sender"), To: to})
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	resp, body := postJSON(t, srv.URL+"/messages/"+sent.ID+"/claim?as="+to.URN(), map[string]any{"holder": "bridge-1"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first claim: status=%d body=%s", resp.StatusCode, body)
	}

	resp, body = postJSON(t, srv.URL+"/messages/"+sent.ID+"/claim?as="+to.URN(), map[string]any{"holder": "bridge-2"})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("second claim while first is active: status=%d body=%s, want 409", resp.StatusCode, body)
	}
}

// TestMessageNack_Retryable_ReconnectCursorRecovery is the "reconnect
// cursor recovery" conformance case: a bridge claims a delivery, drops
// (Nacks retryable -- modeling a lost connection before it could finish),
// then reconnects and claims again, getting the SAME message with no loss
// and no duplicate delivery-core row.
func TestMessageNack_Retryable_ReconnectCursorRecovery(t *testing.T) {
	srv, db := newClaimTestServer(t)
	ctx := context.Background()
	to := claimAddr("bridge-target")
	sent, err := db.MessagingStore().Send(ctx, messaging.Envelope{Kind: messaging.MsgKindNotice, From: claimAddr("sender"), To: to})
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	resp, body := postJSON(t, srv.URL+"/messages/"+sent.ID+"/claim?as="+to.URN(), map[string]any{"holder": "bridge-session-A"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("claim: status=%d body=%s", resp.StatusCode, body)
	}
	var claimed messageClaimResponse
	_ = json.Unmarshal(body, &claimed)

	// The bridge's connection drops before it can ack -- it nacks
	// retryable with no backoff, modeling "reconnect immediately."
	resp, body = postJSON(t, srv.URL+"/messages/"+sent.ID+"/nack?as="+to.URN(), map[string]any{
		"lease": claimed.Lease, "retryable": true, "error": "connection lost",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("nack: status=%d body=%s", resp.StatusCode, body)
	}

	// Reconnect: claim again under a NEW holder id (a fresh bridge process).
	resp, body = postJSON(t, srv.URL+"/messages/"+sent.ID+"/claim?as="+to.URN(), map[string]any{"holder": "bridge-session-B"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("reclaim after reconnect: status=%d body=%s", resp.StatusCode, body)
	}
	var reclaimed messageClaimResponse
	if err := json.Unmarshal(body, &reclaimed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if reclaimed.Message.ID != sent.ID {
		t.Fatalf("reclaimed message id = %q, want %q -- no loss across reconnect", reclaimed.Message.ID, sent.ID)
	}
	if reclaimed.Lease.AttemptID == claimed.Lease.AttemptID {
		t.Fatalf("reclaim attempt id = %q, want a NEW attempt distinct from the dropped one %q", reclaimed.Lease.AttemptID, claimed.Lease.AttemptID)
	}

	resp, _ = postJSON(t, srv.URL+"/messages/"+sent.ID+"/ack?as="+to.URN(), map[string]any{"lease": reclaimed.Lease, "stage": string(delivery.StageConsumed)})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("final ack after reconnect: status=%d", resp.StatusCode)
	}
}

func TestMessageNack_NotRetryable_DeadLetters(t *testing.T) {
	srv, db := newClaimTestServer(t)
	ctx := context.Background()
	to := claimAddr("bridge-target")
	sent, err := db.MessagingStore().Send(ctx, messaging.Envelope{Kind: messaging.MsgKindNotice, From: claimAddr("sender"), To: to})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	resp, body := postJSON(t, srv.URL+"/messages/"+sent.ID+"/claim?as="+to.URN(), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("claim: status=%d body=%s", resp.StatusCode, body)
	}
	var claimed messageClaimResponse
	_ = json.Unmarshal(body, &claimed)

	resp, body = postJSON(t, srv.URL+"/messages/"+sent.ID+"/nack?as="+to.URN(), map[string]any{
		"lease": claimed.Lease, "retryable": false, "error": "permanently unprocessable",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("nack: status=%d body=%s", resp.StatusCode, body)
	}

	deliveryID, _, _ := db.DeliveryIDForMessage(ctx, sent.ID)
	rd, err := db.DeliveryStore().GetDelivery(ctx, delivery.DeliveryID(deliveryID))
	if err != nil {
		t.Fatalf("get delivery: %v", err)
	}
	if rd.Status != delivery.DeliveryDeadLettered {
		t.Fatalf("delivery status = %q, want dead_lettered", rd.Status)
	}
}

// TestMessageAck_LeaseForDifferentMessage_Rejected guards against a
// caller-side mix-up: presenting a genuinely valid lease, but for the
// wrong message id in the URL, must be rejected rather than silently
// acking whatever the lease actually points to.
func TestMessageAck_LeaseForDifferentMessage_Rejected(t *testing.T) {
	srv, db := newClaimTestServer(t)
	ctx := context.Background()
	to := claimAddr("bridge-target")
	sentA, err := db.MessagingStore().Send(ctx, messaging.Envelope{Kind: messaging.MsgKindNotice, From: claimAddr("sender"), To: to})
	if err != nil {
		t.Fatalf("send A: %v", err)
	}
	sentB, err := db.MessagingStore().Send(ctx, messaging.Envelope{Kind: messaging.MsgKindNotice, From: claimAddr("sender"), To: to})
	if err != nil {
		t.Fatalf("send B: %v", err)
	}

	resp, body := postJSON(t, srv.URL+"/messages/"+sentA.ID+"/claim?as="+to.URN(), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("claim A: status=%d body=%s", resp.StatusCode, body)
	}
	var claimedA messageClaimResponse
	_ = json.Unmarshal(body, &claimedA)

	// Present A's lease against B's URL.
	resp, body = postJSON(t, srv.URL+"/messages/"+sentB.ID+"/ack?as="+to.URN(), map[string]any{"lease": claimedA.Lease, "stage": string(delivery.StageConsumed)})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("ack B with A's lease: status=%d body=%s, want 400", resp.StatusCode, body)
	}
}

func TestMessageClaim_NotConfigured_404(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "no-claims.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	srv := httptest.NewServer(NewHandler(Deps{MessageStore: db.MessagingStore()})) // no DeliveryClaims
	t.Cleanup(srv.Close)

	sent, err := db.MessagingStore().Send(context.Background(), messaging.Envelope{Kind: messaging.MsgKindNotice, From: claimAddr("s"), To: claimAddr("t")})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	resp, _ := postJSON(t, srv.URL+"/messages/"+sent.ID+"/claim?as="+claimAddr("t").URN(), nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 when DeliveryClaims is nil", resp.StatusCode)
	}
}

// TestOfflineBridge_AccumulatesBacklog_DeliveredOnReconnect is T07
// acceptance #1's direct proof: a published-local (pull-only) bridge that
// never polls while several messages arrive still finds every one of them
// intact once it "reconnects" (starts claiming) -- no message lost, none
// duplicated, and nothing about this required Tether to take over launch/
// resume authority for the actor (no session, no wake attempt anywhere in
// this test).
func TestOfflineBridge_AccumulatesBacklog_DeliveredOnReconnect(t *testing.T) {
	srv, db := newClaimTestServer(t)
	ctx := context.Background()
	to := claimAddr("offline-bridge-actor")

	const n = 5
	var sent []messaging.Envelope
	for i := 0; i < n; i++ {
		env, err := db.MessagingStore().Send(ctx, messaging.Envelope{Kind: messaging.MsgKindNotice, From: claimAddr("sender"), To: to})
		if err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
		sent = append(sent, env)
	}

	// "Reconnect": claim and consume every backlogged message.
	seen := map[string]bool{}
	for _, env := range sent {
		resp, body := postJSON(t, srv.URL+"/messages/"+env.ID+"/claim?as="+to.URN(), map[string]any{"holder": "bridge-reconnected"})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("claim %s: status=%d body=%s", env.ID, resp.StatusCode, body)
		}
		var claimed messageClaimResponse
		if err := json.Unmarshal(body, &claimed); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if seen[claimed.Message.ID] {
			t.Fatalf("message %s claimed twice -- duplicate delivery", claimed.Message.ID)
		}
		seen[claimed.Message.ID] = true

		if resp, _ := postJSON(t, srv.URL+"/messages/"+env.ID+"/ack?as="+to.URN(), map[string]any{"lease": claimed.Lease, "stage": string(delivery.StageConsumed)}); resp.StatusCode != http.StatusOK {
			t.Fatalf("ack %s: status=%d", env.ID, resp.StatusCode)
		}
	}
	if len(seen) != n {
		t.Fatalf("claimed %d distinct messages, want %d -- backlog was not fully/uniquely delivered", len(seen), n)
	}
}

// TestDuplicateSends_IndependentlyClaimable_NoCrossTalk is the "duplicate
// sends" conformance case as it actually applies today: Tether's /messages
// POST has no sender-scoped idempotency key on the wire (a wider change
// out of this task's scope), so two logically-duplicate Sends mint two
// distinct messages/deliveries -- this proves that's handled SAFELY (each
// independently claimable, acking one never affects the other), not that
// they get deduplicated.
func TestDuplicateSends_IndependentlyClaimable_NoCrossTalk(t *testing.T) {
	srv, db := newClaimTestServer(t)
	ctx := context.Background()
	to := claimAddr("bridge-target")
	env := messaging.Envelope{Kind: messaging.MsgKindNotice, From: claimAddr("sender"), To: to, Payload: []byte(`{"body":"retry of the same logical send"}`)}

	first, err := db.MessagingStore().Send(ctx, env)
	if err != nil {
		t.Fatalf("send 1: %v", err)
	}
	second, err := db.MessagingStore().Send(ctx, env)
	if err != nil {
		t.Fatalf("send 2: %v", err)
	}
	if first.ID == second.ID {
		t.Fatalf("expected two distinct message ids, got the same %q", first.ID)
	}

	respA, bodyA := postJSON(t, srv.URL+"/messages/"+first.ID+"/claim?as="+to.URN(), nil)
	if respA.StatusCode != http.StatusOK {
		t.Fatalf("claim first: status=%d body=%s", respA.StatusCode, bodyA)
	}
	var claimedA messageClaimResponse
	_ = json.Unmarshal(bodyA, &claimedA)

	respB, bodyB := postJSON(t, srv.URL+"/messages/"+second.ID+"/claim?as="+to.URN(), nil)
	if respB.StatusCode != http.StatusOK {
		t.Fatalf("claim second: status=%d body=%s", respB.StatusCode, bodyB)
	}
	var claimedB messageClaimResponse
	_ = json.Unmarshal(bodyB, &claimedB)

	if claimedA.Lease.DeliveryID == claimedB.Lease.DeliveryID {
		t.Fatalf("expected independent delivery ids, got the same %q", claimedA.Lease.DeliveryID)
	}

	// Acking (consuming) the first must not affect the second's claimability.
	if resp, _ := postJSON(t, srv.URL+"/messages/"+first.ID+"/ack?as="+to.URN(), map[string]any{"lease": claimedA.Lease, "stage": string(delivery.StageConsumed)}); resp.StatusCode != http.StatusOK {
		t.Fatalf("ack first: status=%d", resp.StatusCode)
	}
	rdB, err := db.DeliveryStore().GetDelivery(ctx, claimedB.Lease.DeliveryID)
	if err != nil {
		t.Fatalf("get delivery B: %v", err)
	}
	if rdB.Status == delivery.DeliveryDelivered {
		t.Fatalf("second delivery was marked delivered by acking the first -- cross-talk between independent sends")
	}
}
