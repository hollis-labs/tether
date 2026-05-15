package mcpadapter

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

func TestSkillTool_LoadsSkillByID(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	a := newTestAdapter(t)
	a.svc.CatalogRoot = t.TempDir()
	writeSkillFixture(t, a.svc.CatalogRoot, "refactor-go", `---
id: refactor-go
name: Refactor Go
description: Apply Go refactoring patterns.
triggers: [refactor, cleanup]
---

Prefer small, tested changes.
`)

	res := callSkillGetTool(t, a, map[string]any{"skill_id": "refactor-go"})
	if res.IsError {
		t.Fatalf("mux_skill_get returned error: %s", textOf(res))
	}
	body := parseToolJSON(t, res)
	if body["id"] != "refactor-go" {
		t.Fatalf("id = %v", body["id"])
	}
	if body["description"] != "Apply Go refactoring patterns." {
		t.Errorf("description = %v", body["description"])
	}
	if !strings.Contains(body["body"].(string), "Prefer small, tested changes.") {
		t.Errorf("body missing skill content: %v", body["body"])
	}
	if body["layer"] != "system" {
		t.Errorf("layer = %v; want system", body["layer"])
	}
}

func TestSkillTool_ListsSkills(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	a := newTestAdapter(t)
	a.svc.CatalogRoot = t.TempDir()
	writeSkillFixture(t, a.svc.CatalogRoot, "refactor-go", `---
id: refactor-go
name: Refactor Go
description: Apply Go refactoring patterns.
triggers: [refactor, cleanup]
---

Prefer small, tested changes.
`)
	writeSkillFixture(t, a.svc.CatalogRoot, "plan", `---
id: plan
name: Plan
description: Plan work before editing.
---

Plan first.
`)

	res := callSkillTool(t, a, "mux_skill_list", nil)
	if res.IsError {
		t.Fatalf("mux_skill_list returned error: %s", textOf(res))
	}
	body := parseToolJSON(t, res)
	items, ok := body["items"].([]any)
	if !ok {
		t.Fatalf("items = %T; want []any", body["items"])
	}
	if len(items) != 2 {
		t.Fatalf("len(items) = %d; want 2 (%v)", len(items), body["items"])
	}
	first := items[0].(map[string]any)
	if first["id"] != "plan" {
		t.Errorf("items sorted by id; first id = %v", first["id"])
	}
	if _, hasBody := first["body"]; hasBody {
		t.Errorf("mux_skill_list should not return skill body: %v", first)
	}
}

func TestSkillTool_BrokersSkillsByQueryAndTriggers(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	a := newTestAdapter(t)
	a.svc.CatalogRoot = t.TempDir()
	writeSkillFixture(t, a.svc.CatalogRoot, "refactor-go", `---
id: refactor-go
name: Refactor Go
description: Apply Go refactoring patterns.
triggers: [refactor, backend]
priority: 10
---

Prefer small, tested changes.
`)
	writeSkillFixture(t, a.svc.CatalogRoot, "capture-notes", `---
id: capture-notes
name: Capture Notes
description: Capture investigation notes.
triggers: [capture, backend]
priority: 50
---

Write down findings.
`)

	res := callSkillTool(t, a, "mux_skill_broker", map[string]any{
		"query":    "refactor handler",
		"role":     "backend",
		"triggers": "refactor",
		"limit":    2,
	})
	if res.IsError {
		t.Fatalf("mux_skill_broker returned error: %s", textOf(res))
	}
	body := parseToolJSON(t, res)
	items, ok := body["items"].([]any)
	if !ok {
		t.Fatalf("items = %T; want []any", body["items"])
	}
	if len(items) != 2 {
		t.Fatalf("len(items) = %d; want 2", len(items))
	}
	first := items[0].(map[string]any)
	if first["id"] != "refactor-go" {
		t.Fatalf("first id = %v; want refactor-go", first["id"])
	}
	if first["next"] != "mux_skill_get" {
		t.Fatalf("next = %v; want mux_skill_get", first["next"])
	}
	if _, hasBody := first["body"]; hasBody {
		t.Fatalf("mux_skill_broker should not return body: %v", first)
	}
	score := first["score"].(map[string]any)
	if score["query_matches"].(float64) == 0 {
		t.Fatalf("query_matches = %v; want > 0", score["query_matches"])
	}
}

func TestSkillTool_BrokerFiltersByLayer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	a := newTestAdapter(t)
	a.svc.CatalogRoot = t.TempDir()
	writeSkillFixture(t, a.svc.CatalogRoot, "system-skill", `---
id: system-skill
name: System Skill
description: System catalog.
triggers: [backend]
---

System.
`)
	path := filepath.Join(home, ".tether", "skills", "user-skill.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`---
id: user-skill
name: User Skill
description: User catalog.
triggers: [backend]
---

User.
`), 0o644); err != nil {
		t.Fatal(err)
	}

	res := callSkillTool(t, a, "mux_skill_broker", map[string]any{
		"role":   "backend",
		"layers": "user-tether",
	})
	if res.IsError {
		t.Fatalf("mux_skill_broker returned error: %s", textOf(res))
	}
	body := parseToolJSON(t, res)
	items := body["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("len(items) = %d; want 1", len(items))
	}
	if items[0].(map[string]any)["id"] != "user-skill" {
		t.Fatalf("id = %v; want user-skill", items[0].(map[string]any)["id"])
	}
}

func TestSkillTool_AcceptsSlashPrefixedID(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	a := newTestAdapter(t)
	a.svc.CatalogRoot = t.TempDir()
	writeSkillFixture(t, a.svc.CatalogRoot, "plan", "---\nid: plan\nname: Plan\n---\nPlan first.\n")

	res := callSkillGetTool(t, a, map[string]any{"skill_id": "/plan"})
	if res.IsError {
		t.Fatalf("mux_skill_get returned error for slash-prefixed id: %s", textOf(res))
	}
	body := parseToolJSON(t, res)
	if body["id"] != "plan" {
		t.Errorf("id = %v; want plan", body["id"])
	}
}

func TestSkillTool_RejectsPathTraversal(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	a := newTestAdapter(t)
	a.svc.CatalogRoot = t.TempDir()

	res := callSkillGetTool(t, a, map[string]any{"skill_id": "../secret"})
	if !res.IsError {
		t.Fatalf("expected error for path traversal, got %s", textOf(res))
	}
	if !strings.Contains(textOf(res), "not a path") {
		t.Errorf("unexpected error: %s", textOf(res))
	}
}

func TestSkillTool_MissingSkillReturnsNotFound(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	a := newTestAdapter(t)
	a.svc.CatalogRoot = t.TempDir()

	res := callSkillGetTool(t, a, map[string]any{"skill_id": "missing"})
	if !res.IsError {
		t.Fatalf("expected missing skill error, got %s", textOf(res))
	}
	if !strings.Contains(textOf(res), "not_found") {
		t.Errorf("unexpected error: %s", textOf(res))
	}
}

func TestSkillTool_RegisteredWithNativeTools(t *testing.T) {
	a := newTestAdapter(t)
	s := mcpserver.NewMCPServer("test", "0.0.1", mcpserver.WithToolCapabilities(true))
	a.registerTools(s)

	c, err := mcpclient.NewInProcessClient(s)
	if err != nil {
		t.Fatalf("NewInProcessClient: %v", err)
	}
	defer c.Close()
	if _, err := c.Initialize(context.Background(), mcp.InitializeRequest{}); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	resp, err := c.ListTools(context.Background(), mcp.ListToolsRequest{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	found := map[string]bool{}
	for _, tool := range resp.Tools {
		found[tool.Name] = true
	}
	if !found["mux_skill_broker"] {
		t.Fatalf("mux_skill_broker tool was not registered")
	}
	for _, name := range []string{"mux_skill_get", "mux_skill_list"} {
		if !found[name] {
			t.Fatalf("%s tool was not registered", name)
		}
	}
}

func callSkillGetTool(t *testing.T, a *Adapter, args map[string]any) *mcp.CallToolResult {
	return callSkillTool(t, a, "mux_skill_get", args)
}

func callSkillTool(t *testing.T, a *Adapter, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	s := mcpserver.NewMCPServer("test", "0.0.1", mcpserver.WithToolCapabilities(true))
	a.registerSkillTools(s)

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
		t.Fatalf("CallTool: %v", err)
	}
	return res
}

func writeSkillFixture(t *testing.T, root, id, body string) {
	t.Helper()
	path := filepath.Join(root, "skills", id+".md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
