package mcpadapter

import (
	"context"
	"errors"
	"testing"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
)

// mockClient satisfies mcpclient.MCPClient for testing.
type mockClient struct {
	callToolFunc func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error)
}

func (m *mockClient) Initialize(ctx context.Context, req mcp.InitializeRequest) (*mcp.InitializeResult, error) {
	return nil, nil
}
func (m *mockClient) Ping(ctx context.Context) error { return nil }
func (m *mockClient) ListResourcesByPage(ctx context.Context, req mcp.ListResourcesRequest) (*mcp.ListResourcesResult, error) {
	return nil, nil
}
func (m *mockClient) ListResources(ctx context.Context, req mcp.ListResourcesRequest) (*mcp.ListResourcesResult, error) {
	return nil, nil
}
func (m *mockClient) ListResourceTemplatesByPage(ctx context.Context, req mcp.ListResourceTemplatesRequest) (*mcp.ListResourceTemplatesResult, error) {
	return nil, nil
}
func (m *mockClient) ListResourceTemplates(ctx context.Context, req mcp.ListResourceTemplatesRequest) (*mcp.ListResourceTemplatesResult, error) {
	return nil, nil
}
func (m *mockClient) ReadResource(ctx context.Context, req mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	return nil, nil
}
func (m *mockClient) Subscribe(ctx context.Context, req mcp.SubscribeRequest) error { return nil }
func (m *mockClient) Unsubscribe(ctx context.Context, req mcp.UnsubscribeRequest) error {
	return nil
}
func (m *mockClient) ListPromptsByPage(ctx context.Context, req mcp.ListPromptsRequest) (*mcp.ListPromptsResult, error) {
	return nil, nil
}
func (m *mockClient) ListPrompts(ctx context.Context, req mcp.ListPromptsRequest) (*mcp.ListPromptsResult, error) {
	return nil, nil
}
func (m *mockClient) GetPrompt(ctx context.Context, req mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
	return nil, nil
}
func (m *mockClient) ListToolsByPage(ctx context.Context, req mcp.ListToolsRequest) (*mcp.ListToolsResult, error) {
	return nil, nil
}
func (m *mockClient) ListTools(ctx context.Context, req mcp.ListToolsRequest) (*mcp.ListToolsResult, error) {
	return &mcp.ListToolsResult{}, nil
}
func (m *mockClient) CallTool(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if m.callToolFunc != nil {
		return m.callToolFunc(ctx, req)
	}
	return mcp.NewToolResultText("ok"), nil
}
func (m *mockClient) SetLevel(ctx context.Context, req mcp.SetLevelRequest) error { return nil }
func (m *mockClient) Complete(ctx context.Context, req mcp.CompleteRequest) (*mcp.CompleteResult, error) {
	return nil, nil
}
func (m *mockClient) Close() error { return nil }
func (m *mockClient) OnNotification(handler func(mcp.JSONRPCNotification)) {}

var _ mcpclient.MCPClient = (*mockClient)(nil)

func callReq(toolName string) mcp.CallToolRequest {
	return mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: toolName},
	}
}

func TestProxyRouter_SuccessfulForward(t *testing.T) {
	reg := NewToolRegistry()
	mc := &mockClient{
		callToolFunc: func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText("upstream-result"), nil
		},
	}
	reg.Register("upstream", mc, []mcp.Tool{makeTool("upstream_tool")})

	router := NewProxyRouter(reg)
	result, err := router.Handle(context.Background(), callReq("upstream_tool"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Error("expected success result")
	}
}

func TestProxyRouter_ToolNotFound(t *testing.T) {
	reg := NewToolRegistry()
	router := NewProxyRouter(reg)

	result, err := router.Handle(context.Background(), callReq("no_such_tool"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected IsError=true for unknown tool")
	}
}

func TestProxyRouter_DeadUpstream(t *testing.T) {
	reg := NewToolRegistry()
	// Register tool with nil client (simulates dead upstream).
	reg.Register("dead-server", nil, []mcp.Tool{makeTool("dead_tool")})

	router := NewProxyRouter(reg)
	result, err := router.Handle(context.Background(), callReq("dead_tool"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected IsError=true for dead upstream")
	}
}

func TestProxyRouter_NativeTool_ReturnsError(t *testing.T) {
	reg := NewToolRegistry()
	reg.RegisterNative([]mcp.Tool{makeTool("mux_health")})

	router := NewProxyRouter(reg)
	_, err := router.Handle(context.Background(), callReq("mux_health"))
	if err == nil {
		t.Error("expected non-nil error when native tool reaches ProxyRouter")
	}
}

func TestProxyRouter_UpstreamTransportError(t *testing.T) {
	reg := NewToolRegistry()
	mc := &mockClient{
		callToolFunc: func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return nil, errors.New("connection reset")
		},
	}
	reg.Register("flaky", mc, []mcp.Tool{makeTool("flaky_tool")})

	router := NewProxyRouter(reg)
	_, err := router.Handle(context.Background(), callReq("flaky_tool"))
	if err == nil {
		t.Error("expected error propagated from upstream transport failure")
	}
}
