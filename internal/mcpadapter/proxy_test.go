package mcpadapter

import (
	"context"
	"errors"
	"strings"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel/trace"
)

// mockClient satisfies upstreamClient for testing. Far narrower than
// mark3labs' mcpclient.MCPClient (which forced stub methods for prompts,
// resources, completion, etc.) since upstreamClient only asks for the three
// methods client_pool.go/proxy.go actually call.
type mockClient struct {
	callToolFunc  func(ctx context.Context, params *mcpsdk.CallToolParams) (*mcpsdk.CallToolResult, error)
	listToolsFunc func(ctx context.Context, params *mcpsdk.ListToolsParams) (*mcpsdk.ListToolsResult, error)
}

func (m *mockClient) ListTools(ctx context.Context, params *mcpsdk.ListToolsParams) (*mcpsdk.ListToolsResult, error) {
	if m.listToolsFunc != nil {
		return m.listToolsFunc(ctx, params)
	}
	return &mcpsdk.ListToolsResult{}, nil
}
func (m *mockClient) CallTool(ctx context.Context, params *mcpsdk.CallToolParams) (*mcpsdk.CallToolResult, error) {
	if m.callToolFunc != nil {
		return m.callToolFunc(ctx, params)
	}
	return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "ok"}}}, nil
}
func (m *mockClient) Close() error { return nil }

var _ upstreamClient = (*mockClient)(nil)

func callReq(toolName string) ToolCall {
	return ToolCall{ToolName: toolName}
}

func TestProxyRouter_SuccessfulForward(t *testing.T) {
	reg := NewToolRegistry()
	mc := &mockClient{
		callToolFunc: func(_ context.Context, params *mcpsdk.CallToolParams) (*mcpsdk.CallToolResult, error) {
			return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "upstream-result"}}}, nil
		},
	}
	reg.Register("upstream", mc, []*mcpsdk.Tool{makeTool("upstream_tool")})

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
	reg.Register("dead-server", nil, []*mcpsdk.Tool{makeTool("dead_tool")})

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
	reg.RegisterNative([]*mcpsdk.Tool{makeTool("mux_health")})

	router := NewProxyRouter(reg)
	_, err := router.Handle(context.Background(), callReq("mux_health"))
	if err == nil {
		t.Error("expected non-nil error when native tool reaches ProxyRouter")
	}
}

func TestProxyRouter_UpstreamTransportError(t *testing.T) {
	reg := NewToolRegistry()
	mc := &mockClient{
		callToolFunc: func(_ context.Context, _ *mcpsdk.CallToolParams) (*mcpsdk.CallToolResult, error) {
			return nil, errors.New("connection reset")
		},
	}
	reg.Register("flaky", mc, []*mcpsdk.Tool{makeTool("flaky_tool")})

	router := NewProxyRouter(reg)
	_, err := router.Handle(context.Background(), callReq("flaky_tool"))
	if err == nil {
		t.Error("expected error propagated from upstream transport failure")
	}
}

func TestProxyRouter_InjectsTraceContextIntoUpstreamMeta(t *testing.T) {
	reg := NewToolRegistry()
	var gotArgs map[string]any
	var gotMeta map[string]any
	mc := &mockClient{
		callToolFunc: func(_ context.Context, params *mcpsdk.CallToolParams) (*mcpsdk.CallToolResult, error) {
			gotArgs, _ = params.Arguments.(map[string]any)
			gotMeta = map[string]any(params.Meta)
			return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "ok"}}}, nil
		},
	}
	reg.Register("upstream", mc, []*mcpsdk.Tool{makeTool("upstream_tool")})

	router := NewProxyRouter(reg)
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1},
		SpanID:     trace.SpanID{2, 2, 2, 2, 2, 2, 2, 2},
		TraceFlags: trace.FlagsSampled,
		Remote:     false,
	})
	ctx := trace.ContextWithSpanContext(context.Background(), sc)

	result, err := router.Handle(ctx, ToolCall{
		ToolName: "upstream_tool",
		Args:     map[string]any{"input": "hello"},
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
	if tp, _ := gotMeta["_traceparent"].(string); tp == "" {
		t.Errorf("missing _traceparent in _meta: %v", gotMeta)
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
