package mcpadapter

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/mcpgateway"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestSharedLazyStartsDoNotBlockUnrelatedViews(t *testing.T) {
	entries := []config.MCPServerEntry{}
	for _, id := range []string{"slow", "fast", "hidden"} {
		entries = append(entries, config.MCPServerEntry{ID: id, Transport: "http", AllowUnconfinedRemote: true})
	}
	r, err := NewSharedUpstreams(entries, daemonTestRoots(t), true)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	waiting := make(chan struct{})
	release := make(chan struct{})
	var mu sync.Mutex
	counts := map[string]int{}
	r.pool.SetConnectFunc(func(ctx context.Context, e config.MCPServerEntry) (upstreamClient, error) {
		mu.Lock()
		counts[e.ID]++
		mu.Unlock()
		if e.ID == "slow" {
			close(waiting)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return &mockClient{listToolsFunc: func(context.Context, *mcpsdk.ListToolsParams) (*mcpsdk.ListToolsResult, error) {
			return &mcpsdk.ListToolsResult{Tools: []*mcpsdk.Tool{makeTool(e.ID + "_echo")}}, nil
		}}, nil
	})
	done := make(chan error, 1)
	go func() { done <- r.StartOrigins(context.Background(), []string{"slow"}) }()
	<-waiting
	if err := r.StartOrigins(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	fast := make(chan error, 1)
	go func() { fast <- r.StartOrigins(context.Background(), []string{"fast"}) }()
	select {
	case err := <-fast:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("slow origin blocks fast view")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := r.StartOrigins(context.Background(), []string{"slow", "fast"}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if counts["hidden"] != 0 || counts["slow"] != 1 || counts["fast"] != 1 {
		t.Fatal(counts)
	}
}

func TestSharedLazyCollisionDoesNotPoisonHealthyOrigins(t *testing.T) {
	entries := []config.MCPServerEntry{}
	for _, id := range []string{"alpha", "collision", "beta"} {
		entries = append(entries, config.MCPServerEntry{ID: id, Transport: "http", AllowUnconfinedRemote: true})
	}
	r, err := NewSharedUpstreams(entries, daemonTestRoots(t), true)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	r.pool.SetConnectFunc(func(_ context.Context, e config.MCPServerEntry) (upstreamClient, error) {
		name := "same"
		if e.ID == "beta" {
			name = "beta_echo"
		}
		return &mockClient{listToolsFunc: func(context.Context, *mcpsdk.ListToolsParams) (*mcpsdk.ListToolsResult, error) {
			return &mcpsdk.ListToolsResult{Tools: []*mcpsdk.Tool{makeTool(name)}}, nil
		}, callToolFunc: func(context.Context, *mcpsdk.CallToolParams) (*mcpsdk.CallToolResult, error) {
			return &mcpsdk.CallToolResult{}, nil
		}}, nil
	})
	if err := r.StartOrigins(context.Background(), []string{"alpha"}); err != nil {
		t.Fatal(err)
	}
	var collision *mcpgateway.CollisionError
	if err := r.StartOrigins(context.Background(), []string{"collision"}); !errors.As(err, &collision) {
		t.Fatalf("collision: %v", err)
	}
	_ = r.StartOrigins(context.Background(), []string{"beta"})
	view, err := r.OpenView("beta", []string{"beta"}, mcpgateway.Selection{Mode: mcpgateway.Flat})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := view.Service.Dispatch(context.Background(), "beta_echo", nil, nil); err != nil {
		t.Fatal("healthy origin unavailable:", err)
	}
	// Remove the conflicting registration as a successful metadata refresh would.
	if _, err := r.registry.ReplaceServer("alpha", nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := r.registry.ReplaceServer("collision", nil, []*mcpsdk.Tool{makeTool("resolved")}); err != nil {
		t.Fatal(err)
	}
	if err := r.StartOrigins(context.Background(), []string{"beta"}); err != nil {
		t.Fatal("stale collision cached:", err)
	}
}
