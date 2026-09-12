package mcpadapter

// CW-20260907-0026. The placement change and the transition that comes with it.

import (
	"context"
	"testing"

	otelprop "github.com/hollis-labs/go-otel/propagation"
	"github.com/mark3labs/mcp-go/mcp"
	"go.opentelemetry.io/otel/trace"
)

// otelInjectForTest reproduces the PRE-0026 injection shape -- trace context
// written into the arguments map -- using the same go-otel call the old code
// used, so the legacy fixture cannot drift from what older muxes actually send.
func otelInjectForTest(ctx context.Context, args map[string]any) map[string]any {
	return otelprop.InjectMCP(ctx, args)
}

func fakeRemoteContext(t *testing.T) (context.Context, string) {
	t.Helper()
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
	req := injectTraceContextMeta(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "t", Arguments: map[string]any{"input": "x"}},
	})

	got := trace.SpanContextFromContext(extractTraceContext(req))
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
	legacyArgs := map[string]any{"input": "x"}
	legacy := mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "t"}}
	legacy.Params.Arguments = otelInjectForTest(ctx, legacyArgs)
	if _, ok := legacy.GetArguments()["_traceparent"]; !ok {
		t.Fatal("test setup did not produce the legacy shape")
	}

	got := trace.SpanContextFromContext(extractTraceContext(legacy))
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
	req := injectTraceContextMeta(metaCtx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "t"},
	})
	req.Params.Arguments = map[string]any{
		"_traceparent": "00-11111111111111111111111111111111-2222222222222222-01",
	}

	got := trace.SpanContextFromContext(extractTraceContext(req))
	if got.TraceID().String() != wantTraceID {
		t.Errorf("trace id = %s, want the _meta one %s", got.TraceID(), wantTraceID)
	}
}

// An empty or progressToken-only _meta must not be treated as an
// authoritative "no trace context" answer -- it falls through to the legacy
// location instead.
func TestExtractTraceContext_UnrelatedMetaDoesNotShadowLegacy(t *testing.T) {
	ctx, wantTraceID := fakeRemoteContext(t)
	req := mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "t"}}
	req.Params.Meta = &mcp.Meta{ProgressToken: "tok"}
	req.Params.Arguments = otelInjectForTest(ctx, map[string]any{"input": "x"})

	got := trace.SpanContextFromContext(extractTraceContext(req))
	if !got.IsValid() || got.TraceID().String() != wantTraceID {
		t.Errorf("a _meta carrying only a progressToken shadowed the legacy value: got %v", got.TraceID())
	}
}

// No span, no injection, and specifically no empty _meta manufactured.
func TestInjectTraceContextMeta_NoSpanAddsNothing(t *testing.T) {
	req := injectTraceContextMeta(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "t", Arguments: map[string]any{"input": "x"}},
	})
	if req.Params.Meta != nil {
		t.Errorf("manufactured _meta with no active span: %+v", req.Params.Meta)
	}
}
