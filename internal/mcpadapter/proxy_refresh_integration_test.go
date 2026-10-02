package mcpadapter

import (
	"context"
	"testing"
	"time"

	gomcp "github.com/hollis-labs/go-mcp/server"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/mcpgateway"
)

func TestProxyRefresh_UpstreamToolListChangedAddsReachableTool(t *testing.T) {
	ctx := context.Background()

	upstream := gomcp.NewServer("clockwork", "0.0.1")
	upstream.RegisterTool(gomcp.Tool{
		Name:         "clockwork_alpha",
		Description:  "alpha tool",
		InputSchema:  gomcp.EmptyObjectSchema(),
		Handler:      func(context.Context, map[string]any) (any, error) { return "alpha", nil },
		ReadOnlyHint: true,
	})

	upstreamServerTransport, upstreamClientTransport := mcpsdk.NewInMemoryTransports()
	go func() { _ = upstream.SDKServer().Run(context.Background(), upstreamServerTransport) }()
	upstreamSession, err := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test-client", Version: "test"}, nil).Connect(ctx, upstreamClientTransport, nil)
	if err != nil {
		t.Fatalf("connect upstream in-memory client: %v", err)
	}

	registry := NewToolRegistry()
	pool := NewClientPool([]config.MCPServerEntry{{
		ID:        "clockwork",
		Transport: "stdio",
		Command:   "ignored-in-test",
		Tags:      []string{"tasks"},
	}}, registry)
	pool.SetConnectFunc(func(context.Context, config.MCPServerEntry) (upstreamClient, error) {
		return upstreamSession, nil
	})
	defer pool.Shutdown()

	adapter := newTestAdapter(t)
	local := gomcp.NewServer("tether", "test")
	adapter.registerTools(local)

	serverTags := map[string][]string{"clockwork": {"tasks"}}
	allowed := map[string]struct{}{"clockwork": {}}
	router := NewProxyRouter(registry)
	live := &liveProxyCatalog{
		adapter:  adapter,
		server:   local,
		registry: registry,
		router:   router,
		allowed:  allowed,
		firehose: false,
	}
	pool.SetToolRefreshHandler(live.applyRefresh)

	if err := pool.Start(ctx); err != nil {
		t.Fatalf("pool.Start: %v", err)
	}
	live.addProxyTools(registry.AllDefinitions()...)
	gateway := adapter.gatewayService(registry, router, mcpgateway.Selection{Mode: mcpgateway.Search, Source: "test"}, serverTags)
	adapter.registerSearchTool(local, gateway)
	adapter.registerCallTool(local, gateway)
	adapter.registerCatalogRefreshTool(local, pool)

	downstream := connectInMemory(t, local)

	assertToolPresent(ctx, t, downstream, "clockwork_alpha")

	upstream.RegisterTool(gomcp.Tool{
		Name:         "clockwork_beta",
		Description:  "beta tool for inbox ordering",
		InputSchema:  gomcp.EmptyObjectSchema(),
		Handler:      func(context.Context, map[string]any) (any, error) { return "beta", nil },
		ReadOnlyHint: true,
	})

	refreshRes, err := downstream.CallTool(ctx, &mcpsdk.CallToolParams{Name: "tether_catalog_refresh"})
	if err != nil {
		t.Fatalf("CallTool(tether_catalog_refresh): %v", err)
	}
	refreshBody := parseToolJSON(t, refreshRes)
	if ok, _ := refreshBody["ok"].(bool); !ok {
		t.Fatalf("tether_catalog_refresh failed: %v", refreshBody)
	}
	waitForTool(ctx, t, downstream, "clockwork_beta")

	res, err := downstream.CallTool(ctx, &mcpsdk.CallToolParams{Name: "clockwork_beta"})
	if err != nil {
		t.Fatalf("CallTool(clockwork_beta): %v", err)
	}
	if got := textOf(res); got != "beta" {
		t.Fatalf("clockwork_beta result = %q, want %q", got, "beta")
	}

	discoverRes, err := downstream.CallTool(ctx, &mcpsdk.CallToolParams{
		Name:      "tether_tool_search",
		Arguments: map[string]any{"query": "beta inbox ordering"},
	})
	if err != nil {
		t.Fatalf("CallTool(tether_tool_search): %v", err)
	}
	body := parseToolJSON(t, discoverRes)
	tools, _ := body["items"].([]any)
	if len(tools) == 0 {
		t.Fatal("tether_tool_search did not return the refreshed tool")
	}
}

func waitForTool(ctx context.Context, t *testing.T, client *mcpsdk.ClientSession, toolName string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.ListTools(ctx, &mcpsdk.ListToolsParams{})
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

func assertToolPresent(ctx context.Context, t *testing.T, client *mcpsdk.ClientSession, toolName string) {
	t.Helper()
	resp, err := client.ListTools(ctx, &mcpsdk.ListToolsParams{})
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
