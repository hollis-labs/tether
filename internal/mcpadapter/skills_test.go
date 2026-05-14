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

	res := callSkillTool(t, a, map[string]any{"skill_id": "refactor-go"})
	if res.IsError {
		t.Fatalf("Skill returned error: %s", textOf(res))
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

func TestSkillTool_AcceptsSlashPrefixedID(t *testing.T) {
	a := newTestAdapter(t)
	a.svc.CatalogRoot = t.TempDir()
	writeSkillFixture(t, a.svc.CatalogRoot, "plan", "---\nid: plan\nname: Plan\n---\nPlan first.\n")

	res := callSkillTool(t, a, map[string]any{"skill_id": "/plan"})
	if res.IsError {
		t.Fatalf("Skill returned error for slash-prefixed id: %s", textOf(res))
	}
	body := parseToolJSON(t, res)
	if body["id"] != "plan" {
		t.Errorf("id = %v; want plan", body["id"])
	}
}

func TestSkillTool_RejectsPathTraversal(t *testing.T) {
	a := newTestAdapter(t)
	a.svc.CatalogRoot = t.TempDir()

	res := callSkillTool(t, a, map[string]any{"skill_id": "../secret"})
	if !res.IsError {
		t.Fatalf("expected error for path traversal, got %s", textOf(res))
	}
	if !strings.Contains(textOf(res), "not a path") {
		t.Errorf("unexpected error: %s", textOf(res))
	}
}

func TestSkillTool_MissingSkillReturnsNotFound(t *testing.T) {
	a := newTestAdapter(t)
	a.svc.CatalogRoot = t.TempDir()

	res := callSkillTool(t, a, map[string]any{"skill_id": "missing"})
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
	for _, tool := range resp.Tools {
		if tool.Name == "Skill" {
			return
		}
	}
	t.Fatal("Skill tool was not registered")
}

func callSkillTool(t *testing.T, a *Adapter, args map[string]any) *mcp.CallToolResult {
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
	req.Params.Name = "Skill"
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
