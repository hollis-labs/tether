package client

// message_trace_client_test.go — end-to-end coverage for the T09
// MessageTrace/MessageRedrive client methods. Stands up a real
// internal/api.Server over a real *store.Store, matching
// bindings_client_test.go's shape.

import (
	"context"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	messaging "github.com/hollis-labs/go-messaging"
	"github.com/hollis-labs/go-messaging/delivery"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/store"
)

func newMessageTraceTestClient(t *testing.T) (*Client, *store.Store) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "trace-client.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	srv := httptest.NewServer(api.NewHandler(api.Deps{MessageStore: db.MessagingStore(), DeliveryTrace: db, DeliveryRepair: db}))
	t.Cleanup(srv.Close)
	return &Client{baseURL: srv.URL, http: srv.Client()}, db
}

func TestMessageTraceClient_ReturnsAttemptsAndReceipts(t *testing.T) {
	c, db := newMessageTraceTestClient(t)
	ctx := context.Background()
	from := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "sender"}
	to := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "worker"}
	sent, err := db.MessagingStore().Send(ctx, messaging.Envelope{Kind: messaging.MsgKindNotice, From: from, To: to})
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	out, err := c.MessageTrace(ctx, sent.ID)
	if err != nil {
		t.Fatalf("trace: %v", err)
	}
	if out.From != from.URN() || out.To != to.URN() {
		t.Fatalf("from/to = %s/%s, want %s/%s", out.From, out.To, from.URN(), to.URN())
	}
	if out.DeliveryID == "" || out.Status != string(delivery.DeliveryPending) {
		t.Fatalf("trace = %+v, want a pending delivery", out)
	}
}

func TestMessageRedriveClient_DeadLetteredDelivery_Idempotent(t *testing.T) {
	c, db := newMessageTraceTestClient(t)
	ctx := context.Background()
	from := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "sender"}
	to := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "worker"}
	sent, err := db.MessagingStore().Send(ctx, messaging.Envelope{Kind: messaging.MsgKindNotice, From: from, To: to})
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	deliveryID, ok, err := db.DeliveryIDForMessage(ctx, sent.ID)
	if err != nil || !ok {
		t.Fatalf("delivery id: ok=%v err=%v", ok, err)
	}
	ds := db.DeliveryStore()
	claim, err := ds.Claim(ctx, delivery.ClaimRequest{DeliveryID: delivery.DeliveryID(deliveryID), Holder: "s1", LeaseDuration: 30 * time.Second, Nowait: true})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	lease := delivery.LeaseRef{DeliveryID: claim.Attempt.DeliveryID, AttemptID: claim.Attempt.ID, LeaseToken: claim.Attempt.LeaseToken}
	if _, _, err := ds.Nack(ctx, delivery.NackRequest{Lease: lease, Retryable: false, Error: "test"}); err != nil && !errors.Is(err, delivery.ErrDeadLettered) {
		t.Fatalf("nack: %v", err)
	}

	first, err := c.MessageRedrive(ctx, sent.ID, "msg://agent/test/operator", 0)
	if err != nil {
		t.Fatalf("redrive: %v", err)
	}
	if !first.Redriven {
		t.Fatalf("first redrive = %+v, want redriven=true", first)
	}

	second, err := c.MessageRedrive(ctx, sent.ID, "msg://agent/test/operator", 0)
	if err != nil {
		t.Fatalf("redrive (repeat): %v", err)
	}
	if second.Redriven {
		t.Fatalf("second redrive = %+v, want redriven=false (idempotent)", second)
	}
}

func TestMessageRedriveClient_RequiresAuthorizedBy(t *testing.T) {
	c, db := newMessageTraceTestClient(t)
	ctx := context.Background()
	sent, err := db.MessagingStore().Send(ctx, messaging.Envelope{
		Kind: messaging.MsgKindNotice,
		From: messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "s"},
		To:   messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "w"},
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if _, err := c.MessageRedrive(ctx, sent.ID, "", 0); err == nil {
		t.Fatal("expected an error with no authorized_by")
	}
}
