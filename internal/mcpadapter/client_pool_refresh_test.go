package mcpadapter

import (
	"context"
	"errors"
	"strings"
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

func TestClientPool_RefreshCallerCancellationPreservesHealthyStatus(t *testing.T) {
	for _, tc := range []struct {
		name string
		ctx  func() (context.Context, context.CancelFunc)
	}{
		{name: "canceled", ctx: func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx, cancel
		}},
		{name: "deadline", ctx: func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), time.Nanosecond)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &mockClient{listToolsFunc: func(ctx context.Context, _ mcp.ListToolsRequest) (*mcp.ListToolsResult, error) {
				<-ctx.Done()
				return nil, ctx.Err()
			}}
			pool := NewClientPool(nil, NewToolRegistry())
			pool.statuses["healthy"] = &clientStatus{entry: config.MCPServerEntry{ID: "healthy"}, client: client, state: "connected", toolCount: 1}
			ctx, cancel := tc.ctx()
			defer cancel()
			if _, err := pool.RefreshServer(ctx, "healthy"); err == nil {
				t.Fatal("RefreshServer returned nil error for canceled caller")
			}
			status := pool.StatusSummary()[0]
			if status.Status != "connected" || status.Error != "" {
				t.Fatalf("caller cancellation changed healthy status: %+v", status)
			}
		})
	}
}

func TestClientPool_RefreshInternalTimeoutMarksUpstreamFailed(t *testing.T) {
	client := &mockClient{listToolsFunc: func(ctx context.Context, _ mcp.ListToolsRequest) (*mcp.ListToolsResult, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	pool := NewClientPool(nil, NewToolRegistry())
	pool.policy.handshakeTimeout = time.Millisecond
	pool.statuses["stalled"] = &clientStatus{entry: config.MCPServerEntry{ID: "stalled"}, client: client, state: "connected", toolCount: 1}
	if _, err := pool.RefreshServer(context.Background(), "stalled"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("RefreshServer error = %v, want deadline exceeded", err)
	}
	status := pool.StatusSummary()[0]
	if status.Status != "failed" || !strings.Contains(status.Error, "deadline exceeded") {
		t.Fatalf("internal refresh timeout did not mark upstream failed: %+v", status)
	}
}
