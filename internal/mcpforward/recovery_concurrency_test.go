package mcpforward

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/client"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestRecoveryDoesNotWaitForSameForwarderInflightCall(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	started, release := make(chan struct{}), make(chan struct{})
	daemon := mcp.NewServer(&mcp.Implementation{Name: "recovery", Version: "1"}, &mcp.ServerOptions{SupportedProtocolVersions: []string{"2025-11-25"}})
	daemon.AddTool(&mcp.Tool{Name: "slow", InputSchema: map[string]any{"type": "object"}}, func(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return &mcp.CallToolResult{}, nil
	})
	daemon.AddTool(&mcp.Tool{Name: "fast", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{}, nil
	})
	server := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return daemon }, nil))
	defer server.Close()
	relay := &daemonSession{client: client.New("tcp:"+strings.TrimPrefix(server.URL, "http://"), client.WithToken("session"))}
	defer func() { cancel(); close(release); relay.close() }()
	result := make(chan error, 1)
	go func() { _, err := relay.callTool(ctx, &mcp.CallToolParams{Name: "slow"}); result <- err }()
	select {
	case <-started:
	case err := <-result:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// Model the transport's expiry observation without aborting the admitted POST.
	relay.mu.Lock()
	relay.missing.Store(true)
	relay.mu.Unlock()
	probe, stop := context.WithTimeout(ctx, 3*time.Second)
	defer stop()
	if _, err := relay.callTool(probe, &mcp.CallToolParams{Name: "fast"}); err != nil {
		t.Fatal("replacement waited for old in-flight call", err)
	}
	// Let the admitted old call finish only after the replacement has returned.
	release <- struct{}{}
	select {
	case err := <-result:
		if err != nil {
			t.Fatal("replacement retired the admitted call", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}
