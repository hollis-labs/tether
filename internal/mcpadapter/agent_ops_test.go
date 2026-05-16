package mcpadapter

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/config"
)

// newAgentOpsAdapter builds an Adapter wired only with what the agent-ops
// tools need: a catalog root, a (possibly empty) project map, and scopes.
func newAgentOpsAdapter(t *testing.T, catalogRoot string, projects map[string]config.Project, scopes ...string) *Adapter {
	t.Helper()
	svc := &app.Service{
		CatalogRoot: catalogRoot,
		Catalog:     &config.Catalog{Projects: projects},
	}
	return New(svc, "test-token", scopes)
}

func callAgentTool(t *testing.T, a *Adapter, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	s := mcpserver.NewMCPServer("test", "0.0.1", mcpserver.WithToolCapabilities(true))
	a.registerAgentOpsTools(s)

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

// TestAgentOps_CreateListShowEdit drives the full agent-ops surface end to end
// against a temp catalog: create (system scope), list, show, edit, show again.
func TestAgentOps_CreateListShowEdit(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	catalogRoot := t.TempDir()
	a := newAgentOpsAdapter(t, catalogRoot, nil, ScopeCatalogWrite)

	// create
	res := callAgentTool(t, a, "mux_agent_create", map[string]any{
		"id":            "auditor",
		"scope":         "system",
		"name":          "Auditor",
		"roles":         "auditor, reviewer",
		"system_prompt": "you audit code",
	})
	if res.IsError {
		t.Fatalf("create returned error: %s", textOf(res))
	}
	body := parseToolJSON(t, res)
	if body["layer"] != "system" {
		t.Errorf("create layer = %v, want system", body["layer"])
	}
	if _, err := os.Stat(filepath.Join(catalogRoot, "agents", "auditor.yaml")); err != nil {
		t.Fatalf("create did not write the agent file: %v", err)
	}

	// duplicate create rejected — classified as conflict, not internal_error
	dup := callAgentTool(t, a, "mux_agent_create", map[string]any{
		"id": "auditor", "scope": "system",
	})
	if !dup.IsError {
		t.Error("duplicate create: expected error result")
	} else if code := parseToolJSON(t, dup)["code"]; code != "conflict" {
		t.Errorf("duplicate create: code = %v, want conflict", code)
	}

	// list
	listBody := parseToolJSON(t, callAgentTool(t, a, "mux_agent_list", nil))
	agents, _ := listBody["agents"].([]any)
	if len(agents) != 1 {
		t.Fatalf("list returned %d agents, want 1", len(agents))
	}
	first, _ := agents[0].(map[string]any)
	if first["id"] != "auditor" || first["layer"] != "system" {
		t.Errorf("list entry = %v", first)
	}

	// show
	showBody := parseToolJSON(t, callAgentTool(t, a, "mux_agent_show", map[string]any{"id": "auditor"}))
	agent, _ := showBody["agent"].(map[string]any)
	if agent["name"] != "Auditor" {
		t.Errorf("show name = %v, want Auditor", agent["name"])
	}

	// edit — rename, leave system_prompt untouched
	editRes := callAgentTool(t, a, "mux_agent_edit", map[string]any{
		"id": "auditor", "name": "Senior Auditor",
	})
	if editRes.IsError {
		t.Fatalf("edit returned error: %s", textOf(editRes))
	}
	edited, _ := parseToolJSON(t, editRes)["agent"].(map[string]any)
	if edited["name"] != "Senior Auditor" {
		t.Errorf("edited name = %v, want Senior Auditor", edited["name"])
	}
	if edited["system_prompt"] != "you audit code" {
		t.Errorf("edit clobbered system_prompt: %v", edited["system_prompt"])
	}
}

// TestAgentOps_EditClearsRoles confirms an explicitly-passed empty roles
// argument clears the list, while an omitted argument leaves it untouched.
func TestAgentOps_EditClearsRoles(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	a := newAgentOpsAdapter(t, t.TempDir(), nil, ScopeCatalogWrite)

	if res := callAgentTool(t, a, "mux_agent_create", map[string]any{
		"id": "auditor", "scope": "system", "roles": "auditor,reviewer",
	}); res.IsError {
		t.Fatalf("create returned error: %s", textOf(res))
	}

	// Omitting roles leaves them intact.
	keep := parseToolJSON(t, callAgentTool(t, a, "mux_agent_edit", map[string]any{
		"id": "auditor", "name": "Renamed",
	}))
	if roles, _ := keep["agent"].(map[string]any)["roles"].([]any); len(roles) != 2 {
		t.Errorf("omitted roles arg should leave 2 roles, got %v", roles)
	}

	// Passing roles as an empty string clears the list.
	cleared := parseToolJSON(t, callAgentTool(t, a, "mux_agent_edit", map[string]any{
		"id": "auditor", "roles": "",
	}))
	if roles, _ := cleared["agent"].(map[string]any)["roles"].([]any); len(roles) != 0 {
		t.Errorf("empty roles arg should clear the list, got %v", roles)
	}
}

// TestAgentOps_CreateProjectScope writes a project-scoped agent into a
// catalog project's repo root.
func TestAgentOps_CreateProjectScope(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repoRoot := t.TempDir()
	projects := map[string]config.Project{
		"demo": {ID: "demo", RepoRoot: repoRoot},
	}
	a := newAgentOpsAdapter(t, t.TempDir(), projects, ScopeCatalogWrite)

	res := callAgentTool(t, a, "mux_agent_create", map[string]any{
		"id": "demo-builder", "scope": "project", "project": "demo",
	})
	if res.IsError {
		t.Fatalf("create returned error: %s", textOf(res))
	}
	want := filepath.Join(repoRoot, ".tether", "agents", "demo-builder.yaml")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("project-scoped agent not written to %s: %v", want, err)
	}
}

// TestAgentOps_ProjectScopeRequiresProjectArg guards the default scope: a
// create with no scope defaults to project and must demand a project ID.
func TestAgentOps_ProjectScopeRequiresProjectArg(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	a := newAgentOpsAdapter(t, t.TempDir(), nil, ScopeCatalogWrite)

	res := callAgentTool(t, a, "mux_agent_create", map[string]any{"id": "orphan"})
	if !res.IsError {
		t.Fatal("expected error: scope=project create without a project arg")
	}
}

// TestAgentOps_CreateRequiresScope confirms create/edit are gated by
// catalog.write while list/show stay open.
func TestAgentOps_CreateRequiresScope(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	// Adapter holds session.write but NOT catalog.write.
	a := newAgentOpsAdapter(t, t.TempDir(), nil, ScopeSessionWrite)

	res := callAgentTool(t, a, "mux_agent_create", map[string]any{
		"id": "auditor", "scope": "system",
	})
	if !res.IsError {
		t.Fatal("create without catalog.write scope: expected error result")
	}

	// list is read-only and must still work.
	if listRes := callAgentTool(t, a, "mux_agent_list", nil); listRes.IsError {
		t.Errorf("list should not require a scope: %s", textOf(listRes))
	}
}

// TestAgentOps_ShowMissing returns not_found for an unknown agent.
func TestAgentOps_ShowMissing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	a := newAgentOpsAdapter(t, t.TempDir(), nil, ScopeCatalogWrite)

	res := callAgentTool(t, a, "mux_agent_show", map[string]any{"id": "ghost"})
	if !res.IsError {
		t.Fatal("show of unknown agent: expected error result")
	}
}
