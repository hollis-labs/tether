package client

// message_retention_client_test.go — end-to-end coverage for the T09
// MessageRetentionCandidates/MessagePurge client methods. Stands up a
// real internal/api.Server over a real *store.Store, matching
// message_trace_client_test.go's shape.

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"testing"

	messaging "github.com/hollis-labs/go-messaging"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/store"
)

func newMessageRetentionTestClient(t *testing.T) (*Client, *store.Store) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "retention-client.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	srv := httptest.NewServer(api.NewHandler(api.Deps{MessageStore: db.MessagingStore(), Retention: db}))
	t.Cleanup(srv.Close)
	return &Client{baseURL: srv.URL, http: srv.Client()}, db
}

func TestMessageRetentionCandidatesClient_ReturnsCandidates(t *testing.T) {
	c, db := newMessageRetentionTestClient(t)
	ctx := context.Background()
	sent, err := db.MessagingStore().Send(ctx, messaging.Envelope{
		Kind: messaging.MsgKindNotice,
		From: messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "sender"},
		To:   messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "worker"},
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	// Backdate directly so the freshly-sent message clears a realistic
	// older_than_hours cutoff (Send always stamps time.Now()).
	if _, err := db.DB().Exec(`UPDATE messages SET created_at = datetime('now', '-2 days') WHERE id = ?`, sent.ID); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	out, err := c.MessageRetentionCandidates(ctx, 1)
	if err != nil {
		t.Fatalf("candidates: %v", err)
	}
	var found bool
	for _, cand := range out {
		if cand.MessageID == sent.ID {
			found = true
			if cand.Eligible {
				t.Errorf("candidate %+v Eligible=true, want false (delivery still pending)", cand)
			}
		}
	}
	if !found {
		t.Fatalf("candidates = %+v, want to include %s", out, sent.ID)
	}
}

func TestMessagePurgeClient_DeliveredMessage_Idempotent(t *testing.T) {
	c, db := newMessageRetentionTestClient(t)
	ctx := context.Background()
	from := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "sender"}
	to := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "worker"}
	sent, err := db.MessagingStore().Send(ctx, messaging.Envelope{Kind: messaging.MsgKindNotice, From: from, To: to, Payload: []byte(`{"body":"hi"}`)})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if err := db.MessagingStore().Consume(ctx, sent.ID, to); err != nil {
		t.Fatalf("consume: %v", err)
	}

	first, err := c.MessagePurge(ctx, sent.ID, "msg://agent/test/operator")
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if !first.Purged {
		t.Fatalf("first purge = %+v, want purged=true", first)
	}

	second, err := c.MessagePurge(ctx, sent.ID, "msg://agent/test/operator")
	if err != nil {
		t.Fatalf("purge (repeat): %v", err)
	}
	if second.Purged {
		t.Fatalf("second purge = %+v, want purged=false (idempotent)", second)
	}
}

func TestMessagePurgeClient_PendingDelivery_ReturnsError(t *testing.T) {
	c, db := newMessageRetentionTestClient(t)
	ctx := context.Background()
	sent, err := db.MessagingStore().Send(ctx, messaging.Envelope{
		Kind: messaging.MsgKindNotice,
		From: messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "s"},
		To:   messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "w"},
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if _, err := c.MessagePurge(ctx, sent.ID, "msg://agent/test/operator"); err == nil {
		t.Fatal("expected an error purging a message with a pending delivery obligation")
	}
}
