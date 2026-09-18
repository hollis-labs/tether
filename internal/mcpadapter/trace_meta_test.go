package mcpadapter

// CW-20260907-0026. The placement change and the transition that comes with it.

import (
	"context"
	"testing"

	otelprop "github.com/hollis-labs/go-otel/propagation"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel/trace"
)

// otelInjectForTest reproduces the PRE-0026 injection shape -- trace context
// written into the arguments map -- using the same go-otel call the old code
// used, so the legacy fixture cannot drift from what older muxes actually send.
func otelInjectForTest(ctx context.Context, args map[string]any) map[string]any {
	return otelprop.InjectMCP(ctx, args)
}

// fakeRemoteContext builds a remote span context AND installs the tracer and
// propagator the extraction path needs.
//
// recordingTracer is not optional here, and these tests were silently
// depending on another test having installed one: ExtractMCP goes through
// otel.GetTextMapPropagator(), whose default is a no-op that recovers nothing.
// Run in a filter that excluded the Proxied tests, every assertion below
// failed — which is the right outcome, but it means they had been passing on
// global state a sibling file happened to set. Each test now installs its own.
func fakeRemoteContext(t *testing.T) (context.Context, string) {
	t.Helper()
	recordingTracer(t)
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9},
		SpanID:     trace.SpanID{8, 8, 8, 8, 8, 8, 8, 8},
		TraceFlags: trace.FlagsSampled,
	})
	return trace.ContextWithSpanContext(context.Background(), sc), sc.TraceID().String()
}

// A call carrying trace context in _meta establishes the parent span.
func TestExtractTraceContext_ReadsMeta(t *testing.T) {
	ctx, wantTraceID := fakeRemoteContext(t)
	params := &mcpsdk.CallToolParams{Name: "t", Arguments: map[string]any{"input": "x"}}
	injectTraceContextMeta(ctx, params)

	args, _ := params.Arguments.(map[string]any)
	got := trace.SpanContextFromContext(extractTraceContext(map[string]any(params.Meta), args))
	if !got.IsValid() {
		t.Fatal("no span context recovered from _meta")
	}
	if got.TraceID().String() != wantTraceID {
		t.Errorf("trace id = %s, want %s", got.TraceID(), wantTraceID)
	}
}

// THE TRANSITION. Every mux built before this change writes trace context into
// arguments. A newer mux receiving a call from an older one must still
// establish the parent span -- otherwise the link breaks silently, because a
// lost parent and a genuinely-new trace look identical downstream.
//
// Scheduled for removal by CW-20260912-0072, on the condition that every
// deployed mux is past CW-20260907-0026. When that lands, this test goes with
// the fallback.
func TestExtractTraceContext_FallsBackToLegacyArguments(t *testing.T) {
	ctx, wantTraceID := fakeRemoteContext(t)

	// The pre-0026 shape: trace context in arguments, no _meta at all.
	legacyArgs := otelInjectForTest(ctx, map[string]any{"input": "x"})
	if _, ok := legacyArgs["_traceparent"]; !ok {
		t.Fatal("test setup did not produce the legacy shape")
	}

	got := trace.SpanContextFromContext(extractTraceContext(nil, legacyArgs))
	if !got.IsValid() {
		t.Fatal("legacy arguments-side trace context was not recovered; a call from an older mux would silently start a new trace")
	}
	if got.TraceID().String() != wantTraceID {
		t.Errorf("trace id = %s, want %s", got.TraceID(), wantTraceID)
	}
}

// _meta wins when both are present, so a mid-transition call carrying a stale
// arguments-side value does not override the authoritative one.
func TestExtractTraceContext_MetaWinsOverArguments(t *testing.T) {
	metaCtx, wantTraceID := fakeRemoteContext(t)
	params := &mcpsdk.CallToolParams{Name: "t"}
	injectTraceContextMeta(metaCtx, params)
	args := map[string]any{
		"_traceparent": "00-11111111111111111111111111111111-2222222222222222-01",
	}

	got := trace.SpanContextFromContext(extractTraceContext(map[string]any(params.Meta), args))
	if got.TraceID().String() != wantTraceID {
		t.Errorf("trace id = %s, want the _meta one %s", got.TraceID(), wantTraceID)
	}
}

// An empty or progressToken-only _meta must not be treated as an
// authoritative "no trace context" answer -- it falls through to the legacy
// location instead.
func TestExtractTraceContext_UnrelatedMetaDoesNotShadowLegacy(t *testing.T) {
	ctx, wantTraceID := fakeRemoteContext(t)
	meta := map[string]any{"progressToken": "tok"}
	args := otelInjectForTest(ctx, map[string]any{"input": "x"})

	got := trace.SpanContextFromContext(extractTraceContext(meta, args))
	if !got.IsValid() || got.TraceID().String() != wantTraceID {
		t.Errorf("a _meta carrying only a progressToken shadowed the legacy value: got %v", got.TraceID())
	}
}

// No span, no injection, and specifically no empty _meta manufactured.
func TestInjectTraceContextMeta_NoSpanAddsNothing(t *testing.T) {
	params := &mcpsdk.CallToolParams{Name: "t", Arguments: map[string]any{"input": "x"}}
	injectTraceContextMeta(context.Background(), params)
	if params.Meta != nil {
		t.Errorf("manufactured _meta with no active span: %+v", params.Meta)
	}
}
