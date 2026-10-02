package mcpadapter

import (
	"context"
	"fmt"
	"github.com/hollis-labs/tether/internal/mcpgateway"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/config"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type conformanceClient struct {
	*mockClient
	instructions string
}

func (c *conformanceClient) InitializeResult() *mcpsdk.InitializeResult {
	return &mcpsdk.InitializeResult{Instructions: c.instructions}
}

func TestConformanceObservedInstructionsWithoutToolInventoryOrStartupFailure(t *testing.T) {
	registry := NewToolRegistry()
	pool := NewClientPool([]config.MCPServerEntry{{ID: "alpha", Transport: "http"}}, registry)
	pool.SetConnectFunc(func(context.Context, config.MCPServerEntry) (upstreamClient, error) {
		return &conformanceClient{instructions: strings.Repeat("界", 2049), mockClient: &mockClient{listToolsFunc: func(context.Context, *mcpsdk.ListToolsParams) (*mcpsdk.ListToolsResult, error) {
			return &mcpsdk.ListToolsResult{}, nil
		}}}, nil
	})
	if err := pool.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer pool.Shutdown()
	findings, _ := registry.NameDiagnostics()
	if len(findings) != 1 || findings[0].Origin != "alpha" || findings[0].Code != "instructions_length" || findings[0].Name != "" {
		t.Fatalf("initialize observation without tools: %+v", findings)
	}
	registry.RecordInstructions("alpha", 2048)
	findings, _ = registry.NameDiagnostics()
	if len(findings) != 0 {
		t.Fatalf("stale initialization finding: %+v", findings)
	}
}

func TestConformanceCachedOutsideLocksAndCollisionStatusNeverLints(t *testing.T) {
	registry := NewToolRegistry()
	pool := NewClientPool([]config.MCPServerEntry{{ID: "alpha", Transport: "http"}}, registry)
	calls := map[string]int{}
	registry.lintTool = func(origin string, tool *mcpsdk.Tool) []mcpgateway.NameFinding {
		if !registry.mu.TryLock() {
			t.Fatal("linter ran under registry mutex")
		}
		registry.mu.Unlock()
		if !pool.mu.TryLock() {
			t.Fatal("linter ran under pool mutex")
		}
		pool.mu.Unlock()
		calls[tool.Name]++
		return mcpgateway.LintTool(origin, tool)
	}
	pool.SetConnectFunc(func(context.Context, config.MCPServerEntry) (upstreamClient, error) {
		return &mockClient{listToolsFunc: func(context.Context, *mcpsdk.ListToolsParams) (*mcpsdk.ListToolsResult, error) {
			tools := []*mcpsdk.Tool{}
			for i := 0; i < 5; i++ {
				tools = append(tools, makeTool(fmt.Sprintf("alpha_%d", i)))
			}
			return &mcpsdk.ListToolsResult{Tools: tools}, nil
		}}, nil
	})
	if err := pool.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer pool.Shutdown()
	pool.StatusSummary()
	registry.Collisions()
	if len(calls) != 0 {
		t.Fatal("status/collision path invoked linter")
	}
	for i := 0; i < 3; i++ {
		registry.NameDiagnostics()
		pool.StatusSummary()
	}
	if len(calls) != 5 {
		t.Fatalf("calls %+v", calls)
	}
	for name, count := range calls {
		if count != 1 {
			t.Fatalf("%s linted %d times", name, count)
		}
	}
}

func TestConformanceAggregateBudgetIsVisibleAndCached(t *testing.T) {
	registry := NewToolRegistry()
	tools := []*mcpsdk.Tool{}
	// Each schema is legal but expensive. The pass must not resolve all of them.
	for i := 0; i < 20; i++ {
		tool := makeTool(fmt.Sprintf("alpha_%02d", i))
		tool.Annotations = &mcpsdk.ToolAnnotations{}
		tool.InputSchema = map[string]any{"type": "object", "description": strings.Repeat("x", 200*1024)}
		tools = append(tools, tool)
	}
	calls := map[string]int{}
	registry.lintTool = func(origin string, tool *mcpsdk.Tool) []mcpgateway.NameFinding {
		calls[tool.Name]++
		return mcpgateway.LintTool(origin, tool)
	}
	mustRegister(t, registry, "alpha", nil, tools)
	adapter := newTestAdapter(t)
	gateway := adapter.gatewayService(registry, nil, mcpgateway.Selection{Mode: mcpgateway.Flat}, nil)
	status := gateway.Status("")
	if len(calls) != 2 || status.UnexaminedTools != 18 {
		t.Fatalf("budget silently skipped or overran: calls=%d status=%+v", len(calls), status)
	}
	for i := 0; i < 12; i++ {
		gateway.Status("")
	}
	if len(calls) != 20 {
		t.Fatalf("later passes did not examine remainder: %d", len(calls))
	}
	for name, count := range calls {
		if count != 1 {
			t.Fatalf("%s linted %d times", name, count)
		}
	}
	if gateway.Status("").UnexaminedTools != 0 {
		t.Fatal("stale aggregate findings")
	}
}
