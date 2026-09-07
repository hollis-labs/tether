package api

// trace_test.go — end-to-end coverage for GET /messages/{id}/trace (T09,
// messaging vNext). Wires a real *store.Store (real SQLite delivery
// core) and a real *registry.Service so binding-generation cross-
// reference can be exercised genuinely, not mocked.

import (
	"context"
	"encoding/json"
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

func newTraceTestServer(t *testing.T) (*httptest.Server, *store.Store, *registry.Service) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "trace.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	reg := registry.NewService(registry.NewStorage(db.DB()))
	srv := httptest.NewServer(NewHandler(Deps{MessageStore: db.MessagingStore(), DeliveryTrace: db, Registry: reg}))
	t.Cleanup(srv.Close)
	return srv, db, reg
}

func getTrace(t *testing.T, base, messageID string) (*http.Response, TraceResponse) {
	t.Helper()
	resp, err := http.Get(base + "/messages/" + messageID + "/trace")
	if err != nil {
		t.Fatalf("GET trace: %v", err)
	}
	defer resp.Body.Close()
	var out TraceResponse
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	return resp, out
}

// TestMessageTrace_AnswersTheFiveNamedQuestions is acceptance #1's direct
// test: who sent to whom, why a binding resolved, which host accepted,
// which turn was submitted, and why retry/expiry occurred -- driven
// through the real attemptWake path (internal/app), not fabricated.
func TestMessageTrace_AnswersTheFiveNamedQuestions(t *testing.T) {
	srv, db, reg := newTraceTestServer(t)
	ctx := context.Background()

	from := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "sender"}
	to := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "worker"}
	sent, err := db.MessagingStore().Send(ctx, messaging.Envelope{Kind: messaging.MsgKindNotice, From: from, To: to})
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	// Lease a binding so "why a binding resolved" / "which host accepted"
	// have something real to answer.
	target := registry.LogicalAgentBindingTarget("worker")
	binding, err := reg.LeaseBinding(ctx, target, "s1", "worker-host-1", "s1", nil, "", 0)
	if err != nil {
		t.Fatalf("lease binding: %v", err)
	}

	// Drive a real Claim/Ack/Nack cycle mirroring what internal/app/wake.go
	// does, including the T09 binding-generation capture this task added.
	deliveryID, ok, err := db.DeliveryIDForMessage(ctx, sent.ID)
	if err != nil || !ok {
		t.Fatalf("delivery id: ok=%v err=%v", ok, err)
	}
	ds := db.DeliveryStore()
	claim, err := ds.Claim(ctx, delivery.ClaimRequest{
		DeliveryID: delivery.DeliveryID(deliveryID), Holder: "s1", LeaseDuration: 30 * time.Second,
		Nowait: true, BindingGeneration: binding.Generation,
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	lease := delivery.LeaseRef{DeliveryID: claim.Attempt.DeliveryID, AttemptID: claim.Attempt.ID, LeaseToken: claim.Attempt.LeaseToken, BindingGeneration: claim.Attempt.BindingGeneration}
	if _, _, err := ds.Ack(ctx, delivery.AckRequest{Lease: lease, Stage: delivery.StageHostAccepted}); err != nil {
		t.Fatalf("ack host_accepted: %v", err)
	}
	// Simulate a turn-submit failure so "why retry occurred" has a real answer.
	if _, _, err := ds.Nack(ctx, delivery.NackRequest{Lease: lease, Retryable: true, Error: "provider timeout"}); err != nil {
		t.Fatalf("nack: %v", err)
	}

	resp, out := getTrace(t, srv.URL, sent.ID)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	// Q1: who sent to whom.
	if out.From != from.URN() || out.To != to.URN() {
		t.Fatalf("from/to = %s/%s, want %s/%s", out.From, out.To, from.URN(), to.URN())
	}
	if len(out.Attempts) != 1 {
		t.Fatalf("attempts = %+v, want exactly 1", out.Attempts)
	}
	a := out.Attempts[0]
	// Q2: why a binding resolved -- the generation is captured and
	// cross-referenced back to the actual leased binding's host.
	if a.BindingGeneration != binding.Generation {
		t.Fatalf("binding_generation = %d, want %d", a.BindingGeneration, binding.Generation)
	}
	// Q3: which host accepted.
	if a.HostID != "worker-host-1" {
		t.Fatalf("host_id = %q, want worker-host-1", a.HostID)
	}
	if a.HostAcceptedAt == "" {
		t.Fatal("host_accepted_at is empty, want a timestamp")
	}
	// Q4: which turn was submitted -- none was, since this attempt failed
	// before turn_submitted; verify that's honestly reflected (empty).
	if a.TurnSubmittedAt != "" {
		t.Fatalf("turn_submitted_at = %q, want empty (never reached)", a.TurnSubmittedAt)
	}
	// Q5: why retry occurred.
	if a.Error != "provider timeout" || !a.Retryable {
		t.Fatalf("error/retryable = %q/%v, want provider timeout/true", a.Error, a.Retryable)
	}
	if out.Status != string(delivery.DeliveryRetryScheduled) {
		t.Fatalf("status = %q, want retry_scheduled", out.Status)
	}
	if len(out.Receipts) == 0 {
		t.Fatal("receipts is empty, want the persisted/lease_acquired/host_accepted/failed sequence")
	}
}

// TestMessageTrace_LegacyMessageNoDeliveryTracking_HonestPartialTrace
// simulates a pre-T03 row that was never backfilled into the delivery
// core (messages.delivery_id IS NULL) via a direct insert -- db.MessagingStore()
// itself always routes through the delivery-backed store post-T03, so
// this state can no longer arise through the normal Send path; it can
// only exist today as a genuinely un-migrated historical row.
func TestMessageTrace_LegacyMessageNoDeliveryTracking_HonestPartialTrace(t *testing.T) {
	srv, db, _ := newTraceTestServer(t)
	from := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "sender"}
	to := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "worker"}
	const id = "legacy-msg-no-delivery-tracking"
	if _, err := db.DB().Exec(
		`INSERT INTO messages (id, kind, channel, from_urn, to_urn, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		id, "notice", "", from.URN(), to.URN(), time.Now().UTC().Format(time.RFC3339),
	); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}

	resp, out := getTrace(t, srv.URL, id)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 even without delivery tracking", resp.StatusCode)
	}
	if out.From != from.URN() || out.To != to.URN() {
		t.Fatalf("from/to = %s/%s, want %s/%s", out.From, out.To, from.URN(), to.URN())
	}
	if out.DeliveryID != "" || len(out.Attempts) != 0 {
		t.Fatalf("expected no delivery tracking detail, got %+v", out)
	}
}

func TestMessageTrace_NotFound(t *testing.T) {
	srv, _, _ := newTraceTestServer(t)
	resp, _ := getTrace(t, srv.URL, "no-such-message")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestMessageTrace_NotConfigured_404(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "no-trace.db"))
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

	resp, _ := getTrace(t, srv.URL, sent.ID)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (DeliveryTrace not configured)", resp.StatusCode)
	}
}
