package mcpadapter

// CW-20260907-0026. The placement change and the transition that comes with it.

import (
	"context"
	"testing"

	otelprop "github.com/hollis-labs/libs/util/otel/propagation"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel/trace"
)

// otelInjectForTest reproduces the PRE-0026 injection shape -- trace context
// written into the arguments map. The supported metadata helper retains the
// same underscore-prefixed wire keys and traceparent encoding.
func otelInjectForTest(ctx context.Context, args map[string]any) map[string]any {
	return otelprop.InjectMCPMeta(ctx, args)
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
	got := trace.SpanContextFromContext(extractTraceContext(ctx, map[string]any(params.Meta), args))
	if !got.IsValid() {
		t.Fatal("no span context recovered from _meta")
	}
	if got.TraceID().String() != wantTraceID {
		t.Errorf("trace id = %s, want %s", got.TraceID(), wantTraceID)
	}
}

// THE TRANSITION. Every tether built before this change writes trace context into
// arguments. A newer tether receiving a call from an older one must still
// establish the parent span -- otherwise the link breaks silently, because a
// lost parent and a genuinely-new trace look identical downstream.
//
// Scheduled for removal by CW-20260912-0072, on the condition that every
// deployed tether is past CW-20260907-0026. When that lands, this test goes with
// the fallback.
func TestExtractTraceContext_FallsBackToLegacyArguments(t *testing.T) {
	ctx, wantTraceID := fakeRemoteContext(t)

	// The pre-0026 shape: trace context in arguments, no _meta at all.
	legacyArgs := otelInjectForTest(ctx, map[string]any{"input": "x"})
	if _, ok := legacyArgs["_traceparent"]; !ok {
		t.Fatal("test setup did not produce the legacy shape")
	}

	got := trace.SpanContextFromContext(extractTraceContext(ctx, nil, legacyArgs))
	if !got.IsValid() {
		t.Fatal("legacy arguments-side trace context was not recovered; a call from an older tether would silently start a new trace")
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

	got := trace.SpanContextFromContext(extractTraceContext(metaCtx, map[string]any(params.Meta), args))
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

	got := trace.SpanContextFromContext(extractTraceContext(ctx, meta, args))
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

func TestExtractTraceContext_EmptyOrInvalidCarrierDoesNotReuseLocalSpan(t *testing.T) {
	local, _ := fakeRemoteContext(t)
	type requestKey struct{}
	ctx, cancel := context.WithCancel(context.WithValue(local, requestKey{}, "request"))
	cancel()
	for _, tt := range []struct {
		name       string
		meta, args map[string]any
	}{
		{name: "empty"},
		{name: "invalid meta", meta: map[string]any{"_traceparent": "invalid"}},
		{name: "invalid arguments", args: map[string]any{"_traceparent": "invalid"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := extractTraceContext(ctx, tt.meta, tt.args)
			if trace.SpanContextFromContext(got).IsValid() {
				t.Fatal("carrier without a valid parent reused the request's local span")
			}
			if got.Err() != context.Canceled || got.Value(requestKey{}) != "request" {
				t.Fatal("extraction lost request cancellation or values")
			}
		})
	}
}

func TestInjectTraceContextMeta_PreservesMetadataAndRefreshesTraceState(t *testing.T) {
	ctx, wantTraceID := fakeRemoteContext(t)
	state, err := trace.ParseTraceState("vendor=fresh")
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name  string
		state trace.TraceState
	}{
		{name: "no state"},
		{name: "current state", state: state},
	} {
		t.Run(tt.name, func(t *testing.T) {
			requestCtx := trace.ContextWithSpanContext(ctx, trace.SpanContextFromContext(ctx).WithTraceState(tt.state))
			original := mcpsdk.Meta{"progressToken": "token", "_tracestate": "vendor=stale"}
			args := map[string]any{"input": "x"}
			params := &mcpsdk.CallToolParams{Name: "t", Meta: original, Arguments: args}
			injectTraceContextMeta(requestCtx, params)
			if params.Meta["progressToken"] != "token" {
				t.Fatal("injection dropped unrelated metadata")
			}
			if original["_tracestate"] != "vendor=stale" || original["_traceparent"] != nil {
				t.Fatal("injection changed the caller's metadata map")
			}
			if _, exists := args["_traceparent"]; exists {
				t.Fatal("injection wrote trace metadata into tool arguments")
			}
			got := trace.SpanContextFromContext(extractTraceContext(requestCtx, map[string]any(params.Meta), args))
			if got.TraceID().String() != wantTraceID || got.TraceState().String() != tt.state.String() {
				t.Fatalf("injected parent/state = %s/%s, want %s/%s", got.TraceID(), got.TraceState(), wantTraceID, tt.state)
			}
			if tt.state.Len() == 0 {
				if _, exists := params.Meta["_tracestate"]; exists {
					t.Fatal("injection retained a tracestate from the previous parent")
				}
			}
		})
	}
}
