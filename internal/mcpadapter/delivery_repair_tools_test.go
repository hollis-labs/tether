package mcpadapter

// delivery_repair_tools_test.go — end-to-end coverage for
// mux_message_trace / mux_message_redrive (T09). Mirrors
// sanitize_integration_test.go's newTestAdapterWithDaemon pattern (a real
// internal/api HTTP test server wired with DeliveryTrace/DeliveryRepair).

import (
	"context"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	messaging "github.com/hollis-labs/go-messaging"
	"github.com/hollis-labs/go-messaging/delivery"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/store"
)

func newDeliveryRepairTestAdapter(t *testing.T) (*Adapter, *store.Store) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "repair-mcp.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	srv := httptest.NewServer(api.NewHandler(api.Deps{MessageStore: db.MessagingStore(), DeliveryTrace: db, DeliveryRepair: db, Retention: db}))
	t.Cleanup(srv.Close)
	dc := client.New("tcp:" + strings.TrimPrefix(srv.URL, "http://"))

	svc := &app.Service{Store: db}
	a := NewWithDaemon(svc, dc, "test-token", []string{ScopeMessageWrite, ScopeDeliveryWrite})
	return a, db
}

func callDeliveryRepairTool(t *testing.T, a *Adapter, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	s := mcpserver.NewMCPServer("test", "0.0.1", mcpserver.WithToolCapabilities(true))
	a.registerMessageTools(s)
	a.registerDeliveryRepairTools(s)

	c, err := mcpclient.NewInProcessClient(s)
	if err != nil {
		t.Fatalf("NewInProcessClient: %v", err)
	}
	defer c.Close()
	if _, err := c.Initialize(context.Background(), mcp.InitializeRequest{}); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	req := mcp.CallToolRequest{}
	req.Params.Name = name
	if args != nil {
		req.Params.Arguments = args
	}
	res, err := c.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("CallTool %s: %v", name, err)
	}
	return res
}

func TestMessageTraceTool_ReturnsAttempts(t *testing.T) {
	a, db := newDeliveryRepairTestAdapter(t)
	ctx := context.Background()
	from := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "sender"}
	to := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "worker"}
	sent, err := db.MessagingStore().Send(ctx, messaging.Envelope{Kind: messaging.MsgKindNotice, From: from, To: to})
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	res := callDeliveryRepairTool(t, a, "mux_message_trace", map[string]any{"message_id": sent.ID})
	if res.IsError {
		t.Fatalf("trace: %v", res.Content)
	}
	body := parseToolJSON(t, res)
	trace, _ := body["trace"].(map[string]any)
	if trace["from"] != from.URN() || trace["to"] != to.URN() {
		t.Fatalf("trace = %+v, want from/to %s/%s", trace, from.URN(), to.URN())
	}
}

func TestMessageRedriveTool_RequiresScope(t *testing.T) {
	a, db := newDeliveryRepairTestAdapter(t)
	// Rebuild with no scopes to test the gate.
	a.scopes = map[string]struct{}{}
	ctx := context.Background()
	sent, err := db.MessagingStore().Send(ctx, messaging.Envelope{
		Kind: messaging.MsgKindNotice,
		From: messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "s"},
		To:   messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "w"},
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	res := callDeliveryRepairTool(t, a, "mux_message_redrive", map[string]any{
		"message_id": sent.ID, "authorized_by": "msg://agent/test/operator",
	})
	if !res.IsError {
		t.Fatal("expected an error without delivery.write scope")
	}
	if code := parseToolJSON(t, res)["code"]; code != "insufficient_scope" {
		t.Errorf("code = %v, want insufficient_scope", code)
	}
}

func TestMessageRedriveTool_HappyPathAndIdempotent(t *testing.T) {
	a, db := newDeliveryRepairTestAdapter(t)
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

	res := callDeliveryRepairTool(t, a, "mux_message_redrive", map[string]any{
		"message_id": sent.ID, "authorized_by": "msg://agent/test/operator",
	})
	if res.IsError {
		t.Fatalf("redrive: %v", res.Content)
	}
	body := parseToolJSON(t, res)
	result, _ := body["result"].(map[string]any)
	if result["redriven"] != true {
		t.Fatalf("result = %+v, want Redriven=true", result)
	}

	res2 := callDeliveryRepairTool(t, a, "mux_message_redrive", map[string]any{
		"message_id": sent.ID, "authorized_by": "msg://agent/test/operator",
	})
	if res2.IsError {
		t.Fatalf("redrive (repeat): %v", res2.Content)
	}
	body2 := parseToolJSON(t, res2)
	result2, _ := body2["result"].(map[string]any)
	if result2["redriven"] != false {
		t.Fatalf("second redrive result = %+v, want Redriven=false (idempotent)", result2)
	}
}

func TestMessageRetentionCandidatesTool_ListsMessage(t *testing.T) {
	a, db := newDeliveryRepairTestAdapter(t)
	ctx := context.Background()
	sent, err := db.MessagingStore().Send(ctx, messaging.Envelope{
		Kind: messaging.MsgKindNotice,
		From: messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "sender"},
		To:   messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "worker"},
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if _, err := db.DB().Exec(`UPDATE messages SET created_at = datetime('now', '-2 days') WHERE id = ?`, sent.ID); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	res := callDeliveryRepairTool(t, a, "mux_message_retention_candidates", map[string]any{"older_than_hours": float64(1)})
	if res.IsError {
		t.Fatalf("candidates: %v", res.Content)
	}
	body := parseToolJSON(t, res)
	candidates, _ := body["candidates"].([]any)
	var found bool
	for _, c := range candidates {
		m, _ := c.(map[string]any)
		if m["message_id"] == sent.ID {
			found = true
			if m["eligible"] != false {
				t.Errorf("candidate %+v eligible=true, want false (delivery still pending)", m)
			}
		}
	}
	if !found {
		t.Fatalf("candidates = %+v, want to include %s", candidates, sent.ID)
	}
}

func TestMessagePurgeTool_RequiresScope(t *testing.T) {
	a, db := newDeliveryRepairTestAdapter(t)
	a.scopes = map[string]struct{}{}
	ctx := context.Background()
	sent, err := db.MessagingStore().Send(ctx, messaging.Envelope{
		Kind: messaging.MsgKindNotice,
		From: messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "s"},
		To:   messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "w"},
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	res := callDeliveryRepairTool(t, a, "mux_message_purge", map[string]any{
		"message_id": sent.ID, "authorized_by": "msg://agent/test/operator",
	})
	if !res.IsError {
		t.Fatal("expected an error without delivery.write scope")
	}
	if code := parseToolJSON(t, res)["code"]; code != "insufficient_scope" {
		t.Errorf("code = %v, want insufficient_scope", code)
	}
}

func TestMessagePurgeTool_PendingDelivery_ReturnsError(t *testing.T) {
	a, db := newDeliveryRepairTestAdapter(t)
	ctx := context.Background()
	sent, err := db.MessagingStore().Send(ctx, messaging.Envelope{
		Kind: messaging.MsgKindNotice,
		From: messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "s"},
		To:   messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "w"},
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	res := callDeliveryRepairTool(t, a, "mux_message_purge", map[string]any{
		"message_id": sent.ID, "authorized_by": "msg://agent/test/operator",
	})
	if !res.IsError {
		t.Fatal("expected an error purging a message with a pending delivery obligation")
	}
}

func TestMessagePurgeTool_DeliveredMessage_HappyPathAndIdempotent(t *testing.T) {
	a, db := newDeliveryRepairTestAdapter(t)
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

	res := callDeliveryRepairTool(t, a, "mux_message_purge", map[string]any{
		"message_id": sent.ID, "authorized_by": "msg://agent/test/operator",
	})
	if res.IsError {
		t.Fatalf("purge: %v", res.Content)
	}
	body := parseToolJSON(t, res)
	result, _ := body["result"].(map[string]any)
	if result["purged"] != true {
		t.Fatalf("result = %+v, want purged=true", result)
	}

	res2 := callDeliveryRepairTool(t, a, "mux_message_purge", map[string]any{
		"message_id": sent.ID, "authorized_by": "msg://agent/test/operator",
	})
	if res2.IsError {
		t.Fatalf("purge (repeat): %v", res2.Content)
	}
	body2 := parseToolJSON(t, res2)
	result2, _ := body2["result"].(map[string]any)
	if result2["purged"] != false {
		t.Fatalf("second purge result = %+v, want purged=false (idempotent)", result2)
	}
}
