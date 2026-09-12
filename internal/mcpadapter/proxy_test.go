package mcpadapter

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"go.opentelemetry.io/otel/trace"
)

// mockClient satisfies mcpclient.MCPClient for testing.
type mockClient struct {
	callToolFunc  func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error)
	listToolsFunc func(ctx context.Context, req mcp.ListToolsRequest) (*mcp.ListToolsResult, error)

	mu            sync.Mutex
	notifications []func(mcp.JSONRPCNotification)
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
	if m.listToolsFunc != nil {
		return m.listToolsFunc(ctx, req)
	}
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
func (m *mockClient) OnNotification(handler func(mcp.JSONRPCNotification)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.notifications = append(m.notifications, handler)
}

func (m *mockClient) notify(notification mcp.JSONRPCNotification) {
	m.mu.Lock()
	handlers := append([]func(mcp.JSONRPCNotification){}, m.notifications...)
	m.mu.Unlock()
	for _, handler := range handlers {
		handler(notification)
	}
}

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

func TestProxyRouter_InjectsTraceContextIntoUpstreamMeta(t *testing.T) {
	reg := NewToolRegistry()
	var gotArgs map[string]any
	var gotMeta *mcp.Meta
	mc := &mockClient{
		callToolFunc: func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			gotArgs = req.GetArguments()
			gotMeta = req.Params.Meta
			return mcp.NewToolResultText("ok"), nil
		},
	}
	reg.Register("upstream", mc, []mcp.Tool{makeTool("upstream_tool")})

	router := NewProxyRouter(reg)
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1},
		SpanID:     trace.SpanID{2, 2, 2, 2, 2, 2, 2, 2},
		TraceFlags: trace.FlagsSampled,
		Remote:     false,
	})
	ctx := trace.ContextWithSpanContext(context.Background(), sc)

	result, err := router.Handle(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      "upstream_tool",
			Arguments: map[string]any{"input": "hello"},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatal("expected success result")
	}

	if gotMeta == nil {
		t.Fatal("no _meta on the upstream call; trace context has nowhere to ride")
	}
	if tp, _ := gotMeta.AdditionalFields["_traceparent"].(string); tp == "" {
		t.Errorf("missing _traceparent in _meta: %v", gotMeta.AdditionalFields)
	}

	// The point of the whole change: nothing metadata-shaped reaches
	// arguments, so an upstream declaring additionalProperties:false at its
	// schema root has nothing to reject. Asserted as "no underscore keys at
	// all" rather than "no _traceparent", so a future metadata key added to
	// the wrong side fails here too.
	for k := range gotArgs {
		if strings.HasPrefix(k, "_") {
			t.Errorf("underscore key %q reached upstream arguments; a strict schema would reject the whole call", k)
		}
	}
	if gotArgs["input"] != "hello" {
		t.Errorf("payload disturbed: %v", gotArgs)
	}
}
