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
	"go.opentelemetry.io/otel/propagation"
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
//
// It also installs a TextMapPropagator, which production gets from hotel.Init
// (go-otel hotel.go:96). That is needed because INJECTION AND EXTRACTION ARE
// ASYMMETRIC: InjectMCP formats the traceparent string by hand and works with
// no propagator configured, while ExtractMCP goes through
// otel.GetTextMapPropagator() and silently recovers nothing without one. A
// harness that set only the provider would exercise injection honestly and
// quietly no-op every extraction assertion.
func recordingTracer(t *testing.T) {
	t.Helper()
	prevTP := otelGetTracerProvider()
	prevProp := otel.GetTextMapPropagator()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))
	otelSetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		otelSetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
	})
}

// proxiedCallCapture wires a proxy catalog over a mock upstream, registers one
// proxied tool through addProxyTools, and returns the arguments AND the _meta
// the upstream actually received for a call made through a real in-process MCP
// client. Both sides are returned because the assertion that matters is not
// only that trace context arrived but that it arrived in the right half.
func proxiedCallCapture(t *testing.T, toolName string) (map[string]any, *mcp.Meta) {
	t.Helper()

	var received map[string]any
	var receivedMeta *mcp.Meta
	mc := &mockClient{
		callToolFunc: func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			if args, ok := req.Params.Arguments.(map[string]any); ok {
				received = args
			}
			receivedMeta = req.Params.Meta
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
	return received, receivedMeta
}

// The acceptance criterion: a direct proxied call produces a span, and trace
// context reaches the upstream.
func TestProxiedCall_PropagatesTraceContextToUpstream(t *testing.T) {
	recordingTracer(t)

	args, meta := proxiedCallCapture(t, "upstream_traced")

	// CW-20260907-0026 moved the carrier from arguments to _meta. The probe's
	// original finding is unchanged -- a proxied call must be traced -- but the
	// place to look for the evidence moved with it.
	if meta == nil {
		t.Fatal("upstream received no _meta; the proxied path is still untraced")
	}
	if tp, _ := meta.AdditionalFields["_traceparent"].(string); tp == "" {
		t.Fatalf("upstream _meta carried %v with no _traceparent; the proxied path is still untraced", meta.AdditionalFields)
	}
	if args["input"] != "hi" {
		t.Errorf("injection disturbed the payload: %v", args)
	}
}

// Propagation must come from a real recording span, not from something that
// happens to write a header. With a no-op provider the span context is invalid
// and InjectMCP correctly writes nothing -- this pins that the mechanism is
// the span, so a future change that fakes the header without a span fails here.
func TestProxiedCall_NoTracerMeansNoInjection(t *testing.T) {
	args, meta := proxiedCallCapture(t, "upstream_untraced")

	if meta != nil {
		t.Errorf("_meta manufactured with no tracer configured: %v", meta.AdditionalFields)
	}
	if _, ok := args["_traceparent"]; ok {
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
