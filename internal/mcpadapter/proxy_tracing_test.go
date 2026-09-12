package mcpadapter

// proxy_tracing_test.go — CW-20260912-0068.
//
// The bug these lock in: proxied MCP calls created no span, so every call
// every session made to Torque, Tesseract and Cerberus through
// `mux mcp --proxy` was absent from tracing. Tesseract measured it with an
// out-of-process probe that dumped what the upstream actually received:
//
//	direct proxied call  ->  {"keys":["input"]}                 <- nothing injected
//	via mux_call         ->  {"keys":["_traceparent","input"]}  <- injected
//
// These are that probe, in process. They assert on what the UPSTREAM received
// rather than on whether a span object exists, because a span nothing
// propagates from would satisfy the weaker assertion while leaving the
// observable behavior exactly as broken as it was.

import (
	"context"
	"testing"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

func otelGetTracerProvider() trace.TracerProvider   { return otel.GetTracerProvider() }
func otelSetTracerProvider(tp trace.TracerProvider) { otel.SetTracerProvider(tp) }

// otelStartTestSpan starts a span from the currently-installed provider.
func otelStartTestSpan(t *testing.T, name string) (context.Context, trace.Span) {
	t.Helper()
	return otel.Tracer("test").Start(context.Background(), name)
}

// recordingTracer installs a real TracerProvider for the duration of a test
// and restores the previous one.
//
// This is load-bearing rather than ceremony: the global default provider is a
// no-op whose spans carry an INVALID span context, and InjectMCP returns its
// params untouched on an invalid context. Without a real provider the test
// would reproduce the exact bug it is meant to catch and still pass, because
// "no span created" and "span created but not recording" are indistinguishable
// downstream.
func recordingTracer(t *testing.T) {
	t.Helper()
	prev := otelGetTracerProvider()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))
	otelSetTracerProvider(tp)
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		otelSetTracerProvider(prev)
	})
}

// proxiedCallCapture wires a proxy catalog over a mock upstream, registers one
// proxied tool through addProxyTools, and returns the arguments the upstream
// actually received for a call made through a real in-process MCP client.
func proxiedCallCapture(t *testing.T, toolName string) map[string]any {
	t.Helper()

	var received map[string]any
	mc := &mockClient{
		callToolFunc: func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			if args, ok := req.Params.Arguments.(map[string]any); ok {
				received = args
			}
			return mcp.NewToolResultText("ok"), nil
		},
	}

	reg := NewToolRegistry()
	reg.Register("upstream", mc, []mcp.Tool{makeTool(toolName)})

	s := mcpserver.NewMCPServer("test", "0.0.1", mcpserver.WithToolCapabilities(true))
	idx := NewDiscoveryIndex()
	idx.Build(reg, nil)
	live := &liveProxyCatalog{
		adapter:  &Adapter{},
		server:   s,
		registry: reg,
		router:   NewProxyRouter(reg),
		index:    idx,
		firehose: true,
	}
	live.addProxyTools(makeTool(toolName))

	c, err := mcpclient.NewInProcessClient(s)
	if err != nil {
		t.Fatalf("NewInProcessClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	ctx := context.Background()
	if _, err := c.Initialize(ctx, mcp.InitializeRequest{}); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	req := mcp.CallToolRequest{}
	req.Params.Name = toolName
	req.Params.Arguments = map[string]any{"input": "hi"}
	if _, err := c.CallTool(ctx, req); err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if received == nil {
		t.Fatal("upstream never received the call")
	}
	return received
}

// The acceptance criterion: a direct proxied call produces a span, and trace
// context reaches the upstream.
func TestProxiedCall_PropagatesTraceContextToUpstream(t *testing.T) {
	recordingTracer(t)

	got := proxiedCallCapture(t, "upstream_traced")

	if _, ok := got["_traceparent"]; !ok {
		keys := make([]string, 0, len(got))
		for k := range got {
			keys = append(keys, k)
		}
		t.Fatalf("upstream received %v with no _traceparent; the proxied path is still untraced", keys)
	}
	if got["input"] != "hi" {
		t.Errorf("injection disturbed the payload: %v", got)
	}
}

// Propagation must come from a real recording span, not from something that
// happens to write a header. With a no-op provider the span context is invalid
// and InjectMCP correctly writes nothing -- this pins that the mechanism is
// the span, so a future change that fakes the header without a span fails here.
func TestProxiedCall_NoTracerMeansNoInjection(t *testing.T) {
	got := proxiedCallCapture(t, "upstream_untraced")

	if _, ok := got["_traceparent"]; ok {
		t.Error("_traceparent injected with no tracer configured; propagation must derive from a recording span")
	}
}

// mux_call is native (addTool creates its span) and calls router.Handle
// DIRECTLY at proxy_adapter.go's mux_call handler, bypassing the proxied
// handler registered by addProxyTools. So adding a span to the proxied path
// must not give mux_call a second one. "Did this double-count?" is the first
// question worth asking about adding a span to a proxy path, so it is asserted
// rather than reasoned about.
func TestProxyRouter_HandleCreatesNoSpanOfItsOwn(t *testing.T) {
	recordingTracer(t)

	ctx, span := otelStartTestSpan(t, "caller")
	defer span.End()
	callerSpanID := trace.SpanContextFromContext(ctx).SpanID()

	// The upstream reports which span it was called under. If Handle started
	// one of its own, this would be a child id rather than the caller's.
	var innerSpanID trace.SpanID
	reg := NewToolRegistry()
	reg.Register("upstream", &mockClient{
		callToolFunc: func(inner context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			innerSpanID = trace.SpanContextFromContext(inner).SpanID()
			return mcp.NewToolResultText("ok"), nil
		},
	}, []mcp.Tool{makeTool("tool_a")})
	router := NewProxyRouter(reg)

	if _, err := router.Handle(ctx, callReq("tool_a")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if innerSpanID != callerSpanID {
		t.Errorf("Handle started a span of its own (caller %s, upstream saw %s); the proxied span belongs to addProxyTools, and a second one here would double-count mux_call",
			callerSpanID, innerSpanID)
	}
}

// The upstream dimension is what makes a proxied span worth creating -- Torque
// latency has to be separable from Tesseract latency, and the tool name is not
// a reliable substitute (proxy_events records server "mux" for Torque tools).
func TestProxyRouter_RecordsUpstreamServerOnTheSpan(t *testing.T) {
	recordingTracer(t)

	reg := NewToolRegistry()
	reg.Register("tesseract", &mockClient{}, []mcp.Tool{makeTool("tess_tool")})
	router := NewProxyRouter(reg)

	ctx, span := otelStartTestSpan(t, "caller")
	if _, err := router.Handle(ctx, callReq("tess_tool")); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	span.End()

	ro, ok := span.(sdktrace.ReadOnlySpan)
	if !ok {
		t.Fatalf("span is %T, not a ReadOnlySpan", span)
	}
	for _, attr := range ro.Attributes() {
		if string(attr.Key) == "hollis.tool.server" {
			if attr.Value.AsString() != "tesseract" {
				t.Errorf("hollis.tool.server = %q, want tesseract", attr.Value.AsString())
			}
			return
		}
	}
	t.Error("no hollis.tool.server attribute; the span cannot say which upstream it went to")
}
