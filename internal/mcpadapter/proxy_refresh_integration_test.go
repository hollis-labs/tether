package mcpadapter

import (
	"context"
	"testing"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/hollis-labs/tether/internal/config"
)

func TestProxyRefresh_UpstreamToolListChangedAddsReachableTool(t *testing.T) {
	ctx := context.Background()

	upstream := server.NewMCPServer("clockwork", "0.0.1", server.WithToolCapabilities(true))
	upstream.AddTool(mcp.NewTool("clockwork_alpha", mcp.WithDescription("alpha tool")), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("alpha"), nil
	})

	upstreamClient, err := mcpclient.NewInProcessClient(upstream)
	if err != nil {
		t.Fatalf("NewInProcessClient(upstream): %v", err)
	}
	if err := upstreamClient.Start(ctx); err != nil {
		t.Fatalf("upstreamClient.Start: %v", err)
	}

	registry := NewToolRegistry()
	pool := NewClientPool([]config.MCPServerEntry{{
		ID:        "clockwork",
		Transport: "stdio",
		Command:   "ignored-in-test",
		Tags:      []string{"tasks"},
	}}, registry)
	pool.SetConnectFunc(func(context.Context, config.MCPServerEntry) (mcpclient.MCPClient, error) {
		return upstreamClient, nil
	})
	defer pool.Shutdown()

	adapter := newTestAdapter(t)
	local := server.NewMCPServer("agent-mux", "test", server.WithToolCapabilities(true))
	adapter.registerTools(local)

	idx := NewDiscoveryIndex()
	serverTags := map[string][]string{"clockwork": {"tasks"}}
	allowed := map[string]struct{}{"clockwork": {}}
	router := NewProxyRouter(registry)
	live := &liveProxyCatalog{
		adapter:    adapter,
		server:     local,
		registry:   registry,
		router:     router,
		index:      idx,
		serverTags: serverTags,
		allowed:    allowed,
		firehose:   false,
	}
	pool.SetToolRefreshHandler(live.applyRefresh)

	if err := pool.Start(ctx); err != nil {
		t.Fatalf("pool.Start: %v", err)
	}
	idx.Build(registry, serverTags)
	live.addProxyTools(registry.AllDefinitions()...)
	adapter.registerDiscoverTool(local, idx, allowed, false)
	adapter.registerCallTool(local, router)
	adapter.registerCatalogRefreshTool(local, pool)

	downstream, err := mcpclient.NewInProcessClient(local)
	if err != nil {
		t.Fatalf("NewInProcessClient(local): %v", err)
	}
	defer downstream.Close()
	if err := downstream.Start(ctx); err != nil {
		t.Fatalf("downstream.Start: %v", err)
	}

	if _, err := downstream.Initialize(ctx, mcp.InitializeRequest{}); err != nil {
		t.Fatalf("downstream.Initialize: %v", err)
	}

	assertToolPresent(ctx, t, downstream, "clockwork_alpha")

	upstream.AddTool(mcp.NewTool("clockwork_beta", mcp.WithDescription("beta tool for inbox ordering")), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("beta"), nil
	})

	refreshRes, err := downstream.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "mux_catalog_refresh"},
	})
	if err != nil {
		t.Fatalf("CallTool(mux_catalog_refresh): %v", err)
	}
	refreshBody := parseToolJSON(t, refreshRes)
	if ok, _ := refreshBody["ok"].(bool); !ok {
		t.Fatalf("mux_catalog_refresh failed: %v", refreshBody)
	}
	waitForTool(ctx, t, downstream, "clockwork_beta")

	res, err := downstream.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "clockwork_beta"},
	})
	if err != nil {
		t.Fatalf("CallTool(clockwork_beta): %v", err)
	}
	if got := textOf(res); got != "beta" {
		t.Fatalf("clockwork_beta result = %q, want %q", got, "beta")
	}

	discoverRes, err := downstream.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      "mux_discover",
			Arguments: map[string]any{"intent": "beta inbox ordering"},
		},
	})
	if err != nil {
		t.Fatalf("CallTool(mux_discover): %v", err)
	}
	body := parseToolJSON(t, discoverRes)
	tools, _ := body["tools"].([]any)
	if len(tools) == 0 {
		t.Fatal("mux_discover did not return the refreshed tool")
	}
}

func waitForTool(ctx context.Context, t *testing.T, client mcpclient.MCPClient, toolName string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.ListTools(ctx, mcp.ListToolsRequest{})
		if err == nil {
			for _, tool := range resp.Tools {
				if tool.Name == toolName {
					return
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for tool %q", toolName)
}

func assertToolPresent(ctx context.Context, t *testing.T, client mcpclient.MCPClient, toolName string) {
	t.Helper()
	resp, err := client.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	for _, tool := range resp.Tools {
		if tool.Name == toolName {
			return
		}
	}
	t.Fatalf("tool %q not present", toolName)
}
