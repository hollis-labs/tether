package mcpadapter

// group_tools_test.go — end-to-end coverage for the tether_group_*
// MCP tools (T-v060-05-06). Wires a real *registry.Service over an
// in-memory SQLite DB and drives the tools through the in-process MCP
// client. Mirrors registry_tools_test.go's shape.

import (
	"context"
	"database/sql"
	"testing"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/store"
)

func newGroupAdapter(t *testing.T) *Adapter {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := store.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	storage := registry.NewStorage(db)
	svc := registry.NewService(storage)
	return New(&app.Service{Registry: svc}, "test-token", []string{ScopeRegistryWrite, ScopeGroupsWrite})
}

// callGroupTool dispatches a group tool by name through an in-process
// MCP server wired with BOTH the registry and group tool sets — the
// group tests seed agents via tether_registry_register and then drive
// group lifecycle through tether_group_*.
func callGroupTool(t *testing.T, a *Adapter, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	s := mcpserver.NewMCPServer("test", "0.0.1", mcpserver.WithToolCapabilities(true))
	a.registerRegistryTools(s)
	a.registerGroupTools(s)

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

// seedAgentForGroupTest registers an agent and returns the minted URN.
func seedAgentForGroupTest(t *testing.T, a *Adapter, name string) string {
	t.Helper()
	res := callGroupTool(t, a, "tether_registry_register", map[string]any{
		"kind": "agent",
		"profile": map[string]any{
			"display_name": name,
		},
	})
	if res.IsError {
		t.Fatalf("seed agent: %s", textOf(res))
	}
	body := parseToolJSON(t, res)
	prof, ok := body["profile"].(map[string]any)
	if !ok {
		t.Fatalf("missing profile in seed: %v", body)
	}
	urn, _ := prof["urn"].(string)
	if urn == "" {
		t.Fatalf("empty seed URN: %v", prof)
	}
	return urn
}

// seedGroupForGroupTest creates a group whose creator is creatorURN.
func seedGroupForGroupTest(t *testing.T, a *Adapter, name, creatorURN string) string {
	t.Helper()
	res := callGroupTool(t, a, "tether_group_create", map[string]any{
		"display_name": name,
		"creator_urn":  creatorURN,
	})
	if res.IsError {
		t.Fatalf("seed group: %s", textOf(res))
	}
	body := parseToolJSON(t, res)
	prof, ok := body["profile"].(map[string]any)
	if !ok {
		t.Fatalf("missing profile in seed group: %v", body)
	}
	urn, _ := prof["urn"].(string)
	if urn == "" {
		t.Fatalf("empty seed group URN: %v", prof)
	}
	return urn
}

// ─── tether_group_create ────────────────────────────────────────────────

func TestGroupTool_Create_Happy(t *testing.T) {
	a := newGroupAdapter(t)
	creator := seedAgentForGroupTest(t, a, "Creator")
	res := callGroupTool(t, a, "tether_group_create", map[string]any{
		"display_name": "Design Room",
		"creator_urn":  creator,
		"capabilities": []any{"design", "review"},
	})
	if res.IsError {
		t.Fatalf("create: %s", textOf(res))
	}
	body := parseToolJSON(t, res)
	prof, _ := body["profile"].(map[string]any)
	if k, _ := prof["kind"].(string); k != "group" {
		t.Errorf("kind=%q want group", k)
	}
	urn, _ := prof["urn"].(string)
	if !registry.IsGroupURN(urn) {
		t.Errorf("URN %q is not a group URN", urn)
	}
}

func TestGroupTool_Create_MissingFields(t *testing.T) {
	a := newGroupAdapter(t)
	res := callGroupTool(t, a, "tether_group_create", map[string]any{
		"display_name": "Lonely",
		// creator_urn missing
	})
	if !res.IsError {
		t.Fatalf("expected error for missing creator_urn")
	}
	if c := parseToolJSON(t, res)["code"]; c != "invalid_request" {
		t.Errorf("code=%v want invalid_request", c)
	}
}

// ─── tether_group_lookup ────────────────────────────────────────────────

func TestGroupTool_Lookup_NotGroup(t *testing.T) {
	a := newGroupAdapter(t)
	agent := seedAgentForGroupTest(t, a, "X")
	res := callGroupTool(t, a, "tether_group_lookup", map[string]any{"urn": agent})
	if !res.IsError {
		t.Fatalf("expected not_found for agent URN under group tool")
	}
	if c := parseToolJSON(t, res)["code"]; c != "not_found" {
		t.Errorf("code=%v want not_found", c)
	}
}

// ─── tether_group_invite + list_members ────────────────────────────────

func TestGroupTool_InviteAndListMembers(t *testing.T) {
	a := newGroupAdapter(t)
	owner := seedAgentForGroupTest(t, a, "Owner")
	bob := seedAgentForGroupTest(t, a, "Bob")
	grp := seedGroupForGroupTest(t, a, "Crew", owner)

	res := callGroupTool(t, a, "tether_group_invite", map[string]any{
		"group_urn":  grp,
		"member_urn": bob,
		"by":         owner,
	})
	if res.IsError {
		t.Fatalf("invite: %s", textOf(res))
	}

	res = callGroupTool(t, a, "tether_group_list_members", map[string]any{
		"group_urn": grp,
	})
	if res.IsError {
		t.Fatalf("list_members: %s", textOf(res))
	}
	body := parseToolJSON(t, res)
	members, _ := body["members"].([]any)
	if len(members) != 2 {
		t.Errorf("members=%d want 2", len(members))
	}
}

// ─── tether_group_post + tether_group_read ─────────────────────────────

func TestGroupTool_PostAndRead(t *testing.T) {
	a := newGroupAdapter(t)
	owner := seedAgentForGroupTest(t, a, "Owner")
	grp := seedGroupForGroupTest(t, a, "Chatty", owner)
	res := callGroupTool(t, a, "tether_group_post", map[string]any{
		"group_urn": grp,
		"from_urn":  owner,
		"payload":   map[string]any{"text": "first"},
	})
	if res.IsError {
		t.Fatalf("post: %s", textOf(res))
	}
	body := parseToolJSON(t, res)
	if seq, ok := body["group_seq"].(float64); !ok || int(seq) != 1 {
		t.Errorf("group_seq=%v want 1", body["group_seq"])
	}

	// Read.
	res = callGroupTool(t, a, "tether_group_read", map[string]any{
		"group_urn": grp,
		"as":        owner,
	})
	if res.IsError {
		t.Fatalf("read: %s", textOf(res))
	}
	body = parseToolJSON(t, res)
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 1 {
		t.Errorf("messages=%d want 1", len(msgs))
	}
}

// ─── tether_group_post: archived returns locked ──────────────────────────

func TestGroupTool_PostToArchivedIsLocked(t *testing.T) {
	a := newGroupAdapter(t)
	owner := seedAgentForGroupTest(t, a, "Owner")
	grp := seedGroupForGroupTest(t, a, "Closed", owner)
	if res := callGroupTool(t, a, "tether_group_archive", map[string]any{
		"urn": grp, "by": owner,
	}); res.IsError {
		t.Fatalf("archive: %s", textOf(res))
	}
	res := callGroupTool(t, a, "tether_group_post", map[string]any{
		"group_urn": grp,
		"from_urn":  owner,
		"payload":   map[string]any{"text": "after-archive"},
	})
	if !res.IsError {
		t.Fatalf("expected locked error after archive")
	}
	if c := parseToolJSON(t, res)["code"]; c != "locked" {
		t.Errorf("code=%v want locked", c)
	}
}

// ─── tether_group_mark_read ────────────────────────────────────────────

func TestGroupTool_MarkReadAdvancesCursor(t *testing.T) {
	a := newGroupAdapter(t)
	owner := seedAgentForGroupTest(t, a, "Owner")
	grp := seedGroupForGroupTest(t, a, "Cursored", owner)
	for i := 0; i < 3; i++ {
		if res := callGroupTool(t, a, "tether_group_post", map[string]any{
			"group_urn": grp, "from_urn": owner,
			"payload": map[string]any{"i": i},
		}); res.IsError {
			t.Fatalf("post i=%d: %s", i, textOf(res))
		}
	}
	if res := callGroupTool(t, a, "tether_group_mark_read", map[string]any{
		"group_urn": grp, "as": owner, "up_to_seq": 2,
	}); res.IsError {
		t.Fatalf("mark_read: %s", textOf(res))
	}
	res := callGroupTool(t, a, "tether_group_read", map[string]any{
		"group_urn": grp, "as": owner,
	})
	if res.IsError {
		t.Fatalf("read after mark_read: %s", textOf(res))
	}
	body := parseToolJSON(t, res)
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 1 {
		t.Errorf("post-mark-read messages=%d want 1", len(msgs))
	}
}

// ─── tether_group_mentions ─────────────────────────────────────────────

func TestGroupTool_MentionsEmpty(t *testing.T) {
	a := newGroupAdapter(t)
	owner := seedAgentForGroupTest(t, a, "Owner")
	res := callGroupTool(t, a, "tether_group_mentions", map[string]any{
		"as": owner,
	})
	if res.IsError {
		t.Fatalf("mentions: %s", textOf(res))
	}
	body := parseToolJSON(t, res)
	if body["mentions"] == nil {
		t.Errorf("mentions field absent: %v", body)
	}
}
