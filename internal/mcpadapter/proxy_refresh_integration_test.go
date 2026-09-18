package mcpadapter

import (
	"context"
	"testing"
	"time"

	gomcp "github.com/hollis-labs/go-mcp/server"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hollis-labs/tether/internal/config"
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
	local := gomcp.NewServer("agent-mux", "test")
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

	downstream := connectInMemory(t, local)

	assertToolPresent(ctx, t, downstream, "clockwork_alpha")

	upstream.RegisterTool(gomcp.Tool{
		Name:         "clockwork_beta",
		Description:  "beta tool for inbox ordering",
		InputSchema:  gomcp.EmptyObjectSchema(),
		Handler:      func(context.Context, map[string]any) (any, error) { return "beta", nil },
		ReadOnlyHint: true,
	})

	refreshRes, err := downstream.CallTool(ctx, &mcpsdk.CallToolParams{Name: "mux_catalog_refresh"})
	if err != nil {
		t.Fatalf("CallTool(mux_catalog_refresh): %v", err)
	}
	refreshBody := parseToolJSON(t, refreshRes)
	if ok, _ := refreshBody["ok"].(bool); !ok {
		t.Fatalf("mux_catalog_refresh failed: %v", refreshBody)
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
		Name:      "mux_discover",
		Arguments: map[string]any{"intent": "beta inbox ordering"},
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
