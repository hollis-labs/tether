package mcpadapter

import (
	"context"
	"errors"
	"testing"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/hollis-labs/tether/internal/config"
)

func TestClientPool_NotificationRefreshUpdatesRegistry(t *testing.T) {
	ctx := context.Background()
	client := &mockClient{}
	tools := []mcp.Tool{makeTool("clockwork_alpha")}
	client.listToolsFunc = func(context.Context, mcp.ListToolsRequest) (*mcp.ListToolsResult, error) {
		return &mcp.ListToolsResult{Tools: append([]mcp.Tool(nil), tools...)}, nil
	}

	registry := NewToolRegistry()
	pool := NewClientPool([]config.MCPServerEntry{{
		ID:        "clockwork",
		Transport: "stdio",
		Command:   "ignored-in-test",
	}}, registry)
	pool.SetConnectFunc(func(context.Context, config.MCPServerEntry) (mcpclient.MCPClient, error) {
		return client, nil
	})

	refreshCh := make(chan ToolRefreshResult, 1)
	pool.SetToolRefreshHandler(func(res ToolRefreshResult) {
		refreshCh <- res
	})

	if err := pool.Start(ctx); err != nil {
		t.Fatalf("pool.Start: %v", err)
	}
	defer pool.Shutdown()

	if _, ok := registry.Lookup("clockwork_alpha"); !ok {
		t.Fatal("clockwork_alpha not registered after start")
	}

	tools = append(tools, makeTool("clockwork_beta"))
	client.notify(mcp.JSONRPCNotification{
		JSONRPC: mcp.JSONRPC_VERSION,
		Notification: mcp.Notification{
			Method: mcp.MethodNotificationToolsListChanged,
		},
	})

	select {
	case res := <-refreshCh:
		if res.ServerID != "clockwork" {
			t.Fatalf("refresh server = %q, want %q", res.ServerID, "clockwork")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for refresh callback")
	}

	if _, ok := registry.Lookup("clockwork_beta"); !ok {
		t.Fatal("clockwork_beta not registered after list_changed refresh")
	}
}

func TestClientPool_RefreshAllReturnsPartialResults(t *testing.T) {
	ctx := context.Background()

	healthy := &mockClient{}
	healthy.listToolsFunc = func(context.Context, mcp.ListToolsRequest) (*mcp.ListToolsResult, error) {
		return &mcp.ListToolsResult{Tools: []mcp.Tool{makeTool("healthy_alpha")}}, nil
	}

	broken := &mockClient{}
	broken.listToolsFunc = func(context.Context, mcp.ListToolsRequest) (*mcp.ListToolsResult, error) {
		return nil, errors.New("upstream unavailable")
	}

	registry := NewToolRegistry()
	pool := NewClientPool([]config.MCPServerEntry{
		{ID: "healthy", Transport: "stdio", Command: "ignored"},
		{ID: "broken", Transport: "stdio", Command: "ignored"},
	}, registry)
	pool.SetConnectFunc(func(_ context.Context, entry config.MCPServerEntry) (mcpclient.MCPClient, error) {
		if entry.ID == "healthy" {
			return healthy, nil
		}
		return broken, nil
	})

	if err := pool.Start(ctx); err != nil {
		t.Fatalf("pool.Start: %v", err)
	}
	defer pool.Shutdown()

	results, err := pool.RefreshAll(ctx)
	if len(results) != 1 || results[0].ServerID != "healthy" {
		t.Fatalf("results = %#v, want only healthy refresh result", results)
	}
	var refreshErr *RefreshAllError
	if !errors.As(err, &refreshErr) {
		t.Fatalf("err = %v, want RefreshAllError", err)
	}
	if _, ok := refreshErr.Failures["broken"]; !ok {
		t.Fatalf("failures = %#v, want broken server entry", refreshErr.Failures)
	}
}
