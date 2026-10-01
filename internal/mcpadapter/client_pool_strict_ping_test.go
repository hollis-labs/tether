package mcpadapter

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	gomcp "github.com/hollis-labs/go-mcp/server"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hollis-labs/tether/internal/config"
)

// TestConnect_StatelessUpstreamThatRejectsBarePing is the Tether side of
// CW-20261001-0078. A stateless go-sdk v1.8.0 server negotiates protocol
// 2026-07-28 and answers a ping that lacks the SEP-2575 request _meta with
// JSON-RPC -32602; go-sdk v1.8.0's ClientSession.Ping sends exactly that. On
// go-mcp v0.7.0 the handshake ping in connect failed, so the proxy marked the
// upstream down and dropped its tools (tangent, CW-20261001-0003). go-mcp
// v0.14.1 counts a JSON-RPC error reply to ping as reachable.
func TestConnect_StatelessUpstreamThatRejectsBarePing(t *testing.T) {
	upstream := gomcp.NewServer("strict-upstream", "test")
	registerTestTool(upstream, "probe")
	srv := httptest.NewServer(mcpsdk.NewStreamableHTTPHandler(
		func(*http.Request) *mcpsdk.Server { return upstream.SDKServer() },
		&mcpsdk.StreamableHTTPOptions{Stateless: true},
	))
	defer srv.Close()

	pool := NewClientPool(nil, NewToolRegistry())
	ctx := context.Background()
	c, err := pool.connect(ctx, config.MCPServerEntry{ID: "strict", Transport: "http", URL: srv.URL})
	if err != nil {
		t.Fatalf("connect: %v -- a ping the server answered with an error must count as reachable", err)
	}
	defer func() { _ = c.Close() }()

	listed, err := c.ListTools(ctx, &mcpsdk.ListToolsParams{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(listed.Tools) != 1 || listed.Tools[0].Name != "probe" {
		t.Fatalf("tools = %+v; want the upstream's probe tool", listed.Tools)
	}
}
