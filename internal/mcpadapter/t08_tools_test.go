package mcpadapter

// t08_tools_test.go — T08 (messaging vNext, CW-20260906-0039): coverage
// for the new MCP tools this task adds: message claim/ack/nack, registry
// bindings, scoped bindings, whoami, and bootstrap.

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	messaging "github.com/hollis-labs/go-messaging"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/store"
)

// newFullDaemonAdapter wires a real daemon (MessageStore + DeliveryClaims
// + Registry) behind a.client, for tools that need the full cross-cutting
// surface (message claim/ack/nack, whoami, bootstrap) -- unlike
// newTestAdapterWithDaemon (sanitize_integration_test.go), which only
// wires MessageStore.
func newFullDaemonAdapter(t *testing.T) (*Adapter, *store.Store) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "t08-full-daemon.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	reg := registry.NewService(registry.NewStorage(db.DB()))

	srv := httptest.NewServer(api.NewHandler(api.Deps{
		MessageStore:   db.MessagingStore(),
		DeliveryClaims: db,
		Registry:       reg,
	}))
	t.Cleanup(srv.Close)
	dc := client.New("tcp:" + strings.TrimPrefix(srv.URL, "http://"))

	svc := &app.Service{Store: db, Registry: reg}
	return NewWithDaemon(svc, dc, "test-token", []string{ScopeMessageWrite, ScopeRegistryWrite}), db
}

func callTool(t *testing.T, a *Adapter, register func(*Adapter, *mcpserver.MCPServer), name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	s := mcpserver.NewMCPServer("test", "0.0.1", mcpserver.WithToolCapabilities(true))
	register(a, s)
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
	req.Params.Arguments = args
	res, err := c.CallTool(context.Background(), req)
	if err != nil {
		t.Fatalf("CallTool %s: %v", name, err)
	}
	return res
}

// ─── message claim/ack/nack ─────────────────────────────────────────────────

func TestMCPTool_MessageClaimAckRoundTrip(t *testing.T) {
	a, db := newFullDaemonAdapter(t)
	ctx := context.Background()
	to := "msg://agent/test/worker"
	sent, err := db.MessagingStore().Send(ctx, messaging.Envelope{
		Kind: messaging.MsgKindNotice,
		From: messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "sender"},
		To:   messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "worker"},
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	claimRes := callTool(t, a, (*Adapter).registerMessageTools, "mux_message_claim", map[string]any{
		"message_id": sent.ID, "as": to,
	})
	if claimRes.IsError {
		t.Fatalf("claim: %s", textOf(claimRes))
	}
	body := parseToolJSON(t, claimRes)
	claim, ok := body["claim"].(map[string]any)
	if !ok {
		t.Fatalf("claim response missing 'claim': %v", body)
	}
	lease, ok := claim["lease"].(map[string]any)
	if !ok {
		t.Fatalf("claim response missing lease: %v", claim)
	}

	ackRes := callTool(t, a, (*Adapter).registerMessageTools, "mux_message_ack", map[string]any{
		"message_id": sent.ID, "as": to, "stage": "consumed",
		"delivery_id": lease["delivery_id"], "attempt_id": lease["attempt_id"], "lease_token": lease["lease_token"],
	})
	if ackRes.IsError {
		t.Fatalf("ack: %s", textOf(ackRes))
	}
}

func TestMCPTool_MessageClaim_MissingArgs(t *testing.T) {
	a, _ := newFullDaemonAdapter(t)
	res := callTool(t, a, (*Adapter).registerMessageTools, "mux_message_claim", map[string]any{"message_id": "x"})
	if !res.IsError {
		t.Fatalf("expected an error when 'as' is missing")
	}
}

// ─── registry bindings ───────────────────────────────────────────────────────

func TestMCPTool_LeaseBindingRenewRevoke(t *testing.T) {
	a, svc := newRegistryAdapter(t)
	target := "msg://agent/agent-mux/worker"

	leaseRes := callRegistryToolFull(t, a, "tether_registry_lease_binding", map[string]any{
		"target_urn": target, "session_id": "s1", "host_id": "h1", "attempt_id": "a1",
	})
	if leaseRes.IsError {
		t.Fatalf("lease: %s", textOf(leaseRes))
	}
	body := parseToolJSON(t, leaseRes)
	binding, ok := body["binding"].(map[string]any)
	if !ok {
		t.Fatalf("lease response missing binding: %v", body)
	}
	bindingID, _ := binding["id"].(string)
	if bindingID == "" {
		t.Fatalf("binding id empty in response: %v", binding)
	}

	renewRes := callRegistryToolFull(t, a, "tether_registry_renew_binding", map[string]any{"binding_id": bindingID, "ttl_seconds": 60})
	if renewRes.IsError {
		t.Fatalf("renew: %s", textOf(renewRes))
	}

	currentRes := callRegistryToolFull(t, a, "tether_registry_current_binding", map[string]any{"target_urn": target})
	if currentRes.IsError {
		t.Fatalf("current: %s", textOf(currentRes))
	}

	revokeRes := callRegistryToolFull(t, a, "tether_registry_revoke_binding", map[string]any{"binding_id": bindingID})
	if revokeRes.IsError {
		t.Fatalf("revoke: %s", textOf(revokeRes))
	}

	afterRevoke := callRegistryToolFull(t, a, "tether_registry_current_binding", map[string]any{"target_urn": target})
	if !afterRevoke.IsError {
		t.Fatalf("expected not_found after revoke")
	}
	_ = svc
}

func TestMCPTool_LeaseBinding_CannotSupersedeManagedSession(t *testing.T) {
	a, svc := newRegistryAdapter(t)
	target := "msg://agent/agent-mux/worker"
	if _, err := svc.LeaseBinding(context.Background(), target, "real-session", "local", "real-session", nil, registry.VisibilityPrivateLocal, 0); err != nil {
		t.Fatalf("seed managed binding: %v", err)
	}
	res := callRegistryToolFull(t, a, "tether_registry_lease_binding", map[string]any{
		"target_urn": target, "session_id": "bridge-1", "host_id": "h1", "attempt_id": "a1",
	})
	if !res.IsError {
		t.Fatalf("expected a conflict error, got success: %s", textOf(res))
	}
}

// ─── scoped bindings ──────────────────────────────────────────────────────────

func TestMCPTool_ScopedBindingSetResolveHistory(t *testing.T) {
	a, _ := newRegistryAdapter(t)

	setRes := callRegistryToolFull(t, a, "tether_registry_set_scoped_binding", map[string]any{
		"scope": "torque:sprint:X", "slot": "reviewer",
		"target_urns": []any{"msg://agent/agent-mux/agt_a"}, "created_by": "owner",
	})
	if setRes.IsError {
		t.Fatalf("set: %s", textOf(setRes))
	}

	resolveRes := callRegistryToolFull(t, a, "tether_registry_resolve_scoped_binding", map[string]any{
		"scope": "torque:sprint:X", "slot": "reviewer", "single": true,
	})
	if resolveRes.IsError {
		t.Fatalf("resolve single: %s", textOf(resolveRes))
	}
	body := parseToolJSON(t, resolveRes)
	if body["target_urn"] != "msg://agent/agent-mux/agt_a" {
		t.Fatalf("target_urn = %v, want agt_a", body["target_urn"])
	}

	historyRes := callRegistryToolFull(t, a, "tether_registry_scoped_binding_history", map[string]any{
		"scope": "torque:sprint:X", "slot": "reviewer",
	})
	if historyRes.IsError {
		t.Fatalf("history: %s", textOf(historyRes))
	}
}

// ─── whoami / bootstrap ───────────────────────────────────────────────────────

func TestMCPTool_WhoAmI(t *testing.T) {
	a, _ := newFullDaemonAdapter(t)
	res := callTool(t, a, (*Adapter).registerSelfDiscoveryTools, "tether_whoami", map[string]any{
		"urn": "msg://agent/agent-mux/worker",
	})
	if res.IsError {
		t.Fatalf("whoami: %s", textOf(res))
	}
	body := parseToolJSON(t, res)
	if body["ok"] != true {
		t.Fatalf("ok = %v", body["ok"])
	}
}

func TestMCPTool_BootstrapSession(t *testing.T) {
	a, _ := newFullDaemonAdapter(t)
	res := callTool(t, a, (*Adapter).registerSelfDiscoveryTools, "tether_bootstrap_session", map[string]any{
		"session_id": "sess-1", "logical_agent_id": "worker", "publication": "published-local",
	})
	if res.IsError {
		t.Fatalf("bootstrap: %s", textOf(res))
	}
	body := parseToolJSON(t, res)
	bootstrap, ok := body["bootstrap"].(map[string]any)
	if !ok {
		t.Fatalf("bootstrap response missing 'bootstrap': %v", body)
	}
	if bootstrap["registered_now"] != true {
		t.Fatalf("registered_now = %v, want true", bootstrap["registered_now"])
	}
}

func TestMCPTool_BootstrapSession_MissingSessionID(t *testing.T) {
	a, _ := newFullDaemonAdapter(t)
	res := callTool(t, a, (*Adapter).registerSelfDiscoveryTools, "tether_bootstrap_session", map[string]any{
		"logical_agent_id": "worker",
	})
	if !res.IsError {
		t.Fatalf("expected an error when session_id is missing")
	}
}

// callRegistryToolFull mirrors registry_tools_test.go's callRegistryTool
// but also registers the binding/scoped-binding tools this task adds.
func callRegistryToolFull(t *testing.T, a *Adapter, name string, args map[string]any) *mcp.CallToolResult {
	return callTool(t, a, (*Adapter).registerRegistryTools, name, args)
}
