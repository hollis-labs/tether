package mcpadapter

import (
	"encoding/json"
	"testing"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
)

// buildTestRegistry returns a ToolRegistry populated with a small set of
// fake upstream tools spread across two servers.
func buildTestRegistry() *ToolRegistry {
	r := NewToolRegistry()

	clockworkTools := []mcp.Tool{
		{Name: "clockwork_task_create", Description: "Create a new task in the task tracker"},
		{Name: "clockwork_task_list", Description: "List tasks with optional status filters"},
		{Name: "clockwork_sprint_create", Description: "Create a sprint for task cohort management"},
	}
	hadronTools := []mcp.Tool{
		{Name: "hadron_run_enqueue", Description: "Enqueue a blueprint automation run"},
		{Name: "hadron_blueprints_list", Description: "List available automation blueprints"},
	}

	// Use a nil client — discovery only needs the registry metadata.
	r.Register("clockwork", (mcpclient.MCPClient)(nil), clockworkTools)
	r.Register("hadron", (mcpclient.MCPClient)(nil), hadronTools)
	return r
}

func TestDiscoveryIndex_Build(t *testing.T) {
	reg := buildTestRegistry()
	serverTags := map[string][]string{
		"clockwork": {"tasks", "planning"},
		"hadron":    {"automation", "ci"},
	}

	idx := NewDiscoveryIndex()
	idx.Build(reg, serverTags)

	if got := idx.Len(); got != 5 {
		t.Fatalf("expected 5 indexed tools, got %d", got)
	}
}

func TestDiscoveryIndex_SearchByIntent(t *testing.T) {
	reg := buildTestRegistry()
	serverTags := map[string][]string{
		"clockwork": {"tasks", "planning"},
		"hadron":    {"automation", "ci"},
	}
	idx := NewDiscoveryIndex()
	idx.Build(reg, serverTags)

	results := idx.Search("create task", "", nil, 10)
	if len(results) == 0 {
		t.Fatal("expected results for 'create task', got none")
	}
	// "clockwork_task_create" must appear in results (both intent words match).
	found := false
	for _, r := range results {
		if r.ToolName == "clockwork_task_create" {
			found = true
			break
		}
	}
	if !found {
		t.Error("clockwork_task_create not found in 'create task' results")
	}
	// Hadron tools (no matching words) must not appear.
	for _, r := range results {
		if r.ServerID == "hadron" {
			t.Errorf("unexpected hadron tool %q in clockwork-intent results", r.ToolName)
		}
	}
}

func TestDiscoveryIndex_SearchByCategory(t *testing.T) {
	reg := buildTestRegistry()
	serverTags := map[string][]string{
		"clockwork": {"tasks", "planning"},
		"hadron":    {"automation", "ci"},
	}
	idx := NewDiscoveryIndex()
	idx.Build(reg, serverTags)

	results := idx.Search("", "automation", nil, 10)
	if len(results) != 2 {
		t.Fatalf("expected 2 automation tools, got %d", len(results))
	}
	for _, r := range results {
		if r.ServerID != "hadron" {
			t.Errorf("expected server hadron, got %q for tool %q", r.ServerID, r.ToolName)
		}
	}
}

func TestDiscoveryIndex_SearchEmpty_ReturnsAll(t *testing.T) {
	reg := buildTestRegistry()
	idx := NewDiscoveryIndex()
	idx.Build(reg, nil)

	// Empty query with no filters returns up to limit entries.
	results := idx.Search("", "", nil, 10)
	if len(results) != 5 {
		t.Fatalf("empty search: expected 5, got %d", len(results))
	}
}

func TestDiscoveryIndex_SearchLimit(t *testing.T) {
	reg := buildTestRegistry()
	idx := NewDiscoveryIndex()
	idx.Build(reg, nil)

	results := idx.Search("", "", nil, 2)
	if len(results) != 2 {
		t.Fatalf("limit=2: expected 2, got %d", len(results))
	}
}

func TestDiscoveryIndex_SearchResult_HasInputSchema(t *testing.T) {
	reg := NewToolRegistry()
	tool := mcp.NewTool("my_tool",
		mcp.WithDescription("A test tool"),
		mcp.WithString("arg1", mcp.Description("first arg")),
	)
	reg.Register("myserver", (mcpclient.MCPClient)(nil), []mcp.Tool{tool})

	idx := NewDiscoveryIndex()
	idx.Build(reg, nil)

	results := idx.Search("test tool", "", nil, 5)
	if len(results) == 0 {
		t.Fatal("no results for 'test tool'")
	}
	if len(results[0].InputSchema) == 0 {
		t.Error("expected non-empty InputSchema in search result")
	}
	// Verify it's valid JSON.
	var schemaCheck map[string]any
	if err := json.Unmarshal(results[0].InputSchema, &schemaCheck); err != nil {
		t.Errorf("InputSchema is not valid JSON: %v", err)
	}
}

func TestDiscoveryIndex_NoNativeTools(t *testing.T) {
	reg := NewToolRegistry()
	// Register a native tool (no serverID).
	reg.RegisterNative([]mcp.Tool{
		{Name: "mux_health", Description: "Health check"},
	})
	// Register one upstream tool.
	reg.Register("clockwork", (mcpclient.MCPClient)(nil), []mcp.Tool{
		{Name: "clockwork_task_create", Description: "Create task"},
	})

	idx := NewDiscoveryIndex()
	idx.Build(reg, nil)

	// Native tools must not appear in the discovery index.
	if idx.Len() != 1 {
		t.Fatalf("expected 1 indexed tool (upstream only), got %d", idx.Len())
	}
	results := idx.Search("", "", nil, 10)
	for _, r := range results {
		if r.ToolName == "mux_health" {
			t.Error("native tool mux_health must not appear in discovery results")
		}
	}
}
