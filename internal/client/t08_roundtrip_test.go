package client

// t08_roundtrip_test.go — T08 (messaging vNext, CW-20260906-0039):
// round-trip coverage for the client methods this task adds, against a
// REAL internal/api.Server + real SQLite (not the mockDaemon fixture used
// elsewhere in this package) -- "equivalent operations and errors
// round-trip across all supported surfaces" (acceptance #1) is best
// proven end to end, the same style established in T05/T07's own
// real-daemon tests.

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	messaging "github.com/hollis-labs/go-messaging"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/store"
)

func newT08RealDaemon(t *testing.T) (*Client, *store.Store, *registry.Service) {
	t.Helper()
	db, err := store.Open(t.TempDir() + "/t08.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	reg := registry.NewService(registry.NewStorage(db.DB()))
	srv := httptest.NewServer(api.NewHandler(api.Deps{
		MessageStore:   db.MessagingStore(),
		DeliveryClaims: db,
		Registry:       reg,
	}))
	t.Cleanup(srv.Close)
	c := New("tcp:" + strings.TrimPrefix(srv.URL, "http://"))
	return c, db, reg
}

func TestClient_MessageClaimAckRoundTrip(t *testing.T) {
	c, db, _ := newT08RealDaemon(t)
	ctx := context.Background()
	to := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "worker"}
	sent, err := db.MessagingStore().Send(ctx, messaging.Envelope{Kind: messaging.MsgKindNotice, From: messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "sender"}, To: to})
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	claimed, err := c.MessageClaim(ctx, sent.ID, to.URN(), "bridge-1", 30)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed.Message.ID != sent.ID || claimed.Lease.DeliveryID == "" {
		t.Fatalf("claimed = %+v", claimed)
	}
	if err := c.MessageAck(ctx, sent.ID, to.URN(), claimed.Lease, "consumed"); err != nil {
		t.Fatalf("ack: %v", err)
	}
}

func TestClient_MessageNackRoundTrip(t *testing.T) {
	c, db, _ := newT08RealDaemon(t)
	ctx := context.Background()
	to := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "worker"}
	sent, err := db.MessagingStore().Send(ctx, messaging.Envelope{Kind: messaging.MsgKindNotice, From: messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "sender"}, To: to})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	claimed, err := c.MessageClaim(ctx, sent.ID, to.URN(), "", 0)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := c.MessageNack(ctx, sent.ID, to.URN(), claimed.Lease, true, "retry me", 0); err != nil {
		t.Fatalf("nack: %v", err)
	}
}

func TestClient_BindingLeaseRenewRevokeRoundTrip(t *testing.T) {
	c, _, _ := newT08RealDaemon(t)
	ctx := context.Background()
	target := "msg://agent/agent-mux/worker"

	b, err := c.Registry().LeaseBinding(ctx, target, "s1", "external-bridge", "a1", []string{"pull-only"}, 0)
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	if b.SessionID != "s1" || b.Visibility != registry.VisibilityPublishedLocal {
		t.Fatalf("leased binding = %+v", b)
	}

	renewed, err := c.Registry().RenewBindingLease(ctx, b.ID, 60*time.Second)
	if err != nil {
		t.Fatalf("renew: %v", err)
	}
	if renewed.ID != b.ID {
		t.Fatalf("renewed = %+v", renewed)
	}

	current, err := c.Registry().CurrentBinding(ctx, target)
	if err != nil || current.ID != b.ID {
		t.Fatalf("current = %+v, err=%v", current, err)
	}

	rows, err := c.Registry().ListBindings(ctx, target)
	if err != nil || len(rows) != 1 {
		t.Fatalf("list = %+v, err=%v", rows, err)
	}

	if err := c.Registry().RevokeBinding(ctx, b.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := c.Registry().CurrentBinding(ctx, target); err == nil {
		t.Fatalf("expected no current binding after revoke")
	}
}

func TestClient_ScopedBindingRoundTrip(t *testing.T) {
	c, _, _ := newT08RealDaemon(t)
	ctx := context.Background()

	b, err := c.Registry().SetScopedBinding(ctx, "torque:sprint:X", "reviewer", []string{"msg://agent/agent-mux/agt_a"}, nil, "owner")
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if b.Revision != 1 {
		t.Fatalf("revision = %d, want 1", b.Revision)
	}

	resolved, err := c.Registry().ResolveScopedBinding(ctx, "torque:sprint:X", "reviewer")
	if err != nil || len(resolved.TargetURNs) != 1 {
		t.Fatalf("resolved = %+v, err=%v", resolved, err)
	}

	target, _, err := c.Registry().ResolveScopedBindingSingle(ctx, "torque:sprint:X", "reviewer")
	if err != nil || target != "msg://agent/agent-mux/agt_a" {
		t.Fatalf("single = %q, err=%v", target, err)
	}

	history, err := c.Registry().ListScopedBindingRevisions(ctx, "torque:sprint:X", "reviewer")
	if err != nil || len(history) != 1 {
		t.Fatalf("history = %+v, err=%v", history, err)
	}
}

func TestClient_WhoAmIRoundTrip(t *testing.T) {
	c, _, reg := newT08RealDaemon(t)
	ctx := context.Background()
	target := "msg://agent/agent-mux/worker"
	if _, err := reg.LeaseBinding(ctx, target, "s1", "h1", "a1", []string{"pull-only"}, registry.VisibilityPublishedLocal, 0); err != nil {
		t.Fatalf("seed binding: %v", err)
	}

	out, err := c.WhoAmI(ctx, target)
	if err != nil {
		t.Fatalf("whoami: %v", err)
	}
	if out.Binding == nil || out.Binding.SessionID != "s1" {
		t.Fatalf("whoami binding = %+v", out.Binding)
	}
}

func TestClient_BootstrapSessionRoundTrip(t *testing.T) {
	c, _, _ := newT08RealDaemon(t)
	ctx := context.Background()

	out, err := c.BootstrapSession(ctx, BootstrapSessionRequest{
		SessionID: "sess-1", LogicalAgentID: "worker", Publication: "published-local",
	})
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if !out.RegisteredNow || out.Binding == nil || out.Binding.SessionID != "sess-1" {
		t.Fatalf("bootstrap result = %+v", out)
	}

	// Repeated hook call: idempotent.
	again, err := c.BootstrapSession(ctx, BootstrapSessionRequest{
		SessionID: "sess-1", LogicalAgentID: "worker", Publication: "published-local",
	})
	if err != nil {
		t.Fatalf("bootstrap (repeat): %v", err)
	}
	if !again.ReusedBinding {
		t.Fatalf("expected the repeated call to report ReusedBinding, got %+v", again)
	}
}
