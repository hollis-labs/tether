package mcpadapter

import (
	"context"
	"errors"
	"github.com/hollis-labs/tether/internal/callcontext"
	"github.com/hollis-labs/tether/internal/mcpgateway"
	"testing"

	gomcp "github.com/hollis-labs/go-mcp/server"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel/trace"
)

// TestProvenance_DirectProxiedCallStampsEnvelope tests that a direct proxied tool call
// stamps tether.provenance into params._meta with schema_version, session_id, and workstream_id.
func TestProvenance_DirectProxiedCallStampsEnvelope(t *testing.T) {
	var gotMeta map[string]any
	var gotArgs map[string]any

	mc := &mockClient{
		callToolFunc: func(_ context.Context, params *mcpsdk.CallToolParams) (*mcpsdk.CallToolResult, error) {
			gotMeta = map[string]any(params.Meta)
			gotArgs, _ = params.Arguments.(map[string]any)
			return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "ok"}}}, nil
		},
	}

	reg := NewToolRegistry()
	reg.Register("tesseract", mc, []*mcpsdk.Tool{makeTool("workspace_write")})

	router := NewProxyRouter(reg)

	ctx := callcontext.WithSnapshot(context.Background(), callcontext.Snapshot{Verified: true, SessionID: "sess-100", WorkstreamID: "ws-200"})
	call := ToolCall{
		ToolName: "workspace_write",
		Args:     map[string]any{"item_id": "item-1", "summary": "draft"},
	}

	res, err := router.Handle(ctx, call)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected tool error: %v", res)
	}

	if gotMeta == nil {
		t.Fatal("expected _meta on forwarded call, got nil")
	}
	env := ExtractProvenanceMeta(gotMeta)
	if env == nil {
		t.Fatalf("missing tether.provenance in _meta: %v", gotMeta)
	}
	if env.SchemaVersion != ProvenanceSchemaVersion {
		t.Errorf("schema_version = %d, want 2", env.SchemaVersion)
	}
	if env.SessionID != "sess-100" {
		t.Errorf("session_id = %q, want %q", env.SessionID, "sess-100")
	}
	if env.WorkstreamID != "ws-200" {
		t.Errorf("workstream_id = %q, want %q", env.WorkstreamID, "ws-200")
	}

	// Verify ordinary arguments are untouched.
	if gotArgs["item_id"] != "item-1" || gotArgs["summary"] != "draft" {
		t.Errorf("arguments altered: %v", gotArgs)
	}
}

// TestProvenance_TetherCallForwardingStampsEnvelope tests that forwarding through tether_tool_call
// stamps tether.provenance onto the upstream request.
func TestProvenance_TetherCallForwardingStampsEnvelope(t *testing.T) {
	var gotMeta map[string]any
	var gotArgs map[string]any

	mc := &mockClient{
		callToolFunc: func(_ context.Context, params *mcpsdk.CallToolParams) (*mcpsdk.CallToolResult, error) {
			gotMeta = map[string]any(params.Meta)
			gotArgs, _ = params.Arguments.(map[string]any)
			return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "ok"}}}, nil
		},
	}

	reg := NewToolRegistry()
	reg.Register("tesseract", mc, []*mcpsdk.Tool{makeTool("knowledge_write")})

	router := NewProxyRouter(reg)

	a := New(nil, "", nil)
	a.SessionID = "claimed-session"
	a.SetCallerContextResolver(func(context.Context) (callcontext.Snapshot, error) {
		return callcontext.Snapshot{Verified: true, SessionID: "sess-mc", WorkstreamID: "ws-mc"}, nil
	})

	s := gomcp.NewServer("test-tether", "0.0.1")
	a.registerCallTool(s, a.gatewayService(reg, router, mcpgateway.Selection{Mode: mcpgateway.Search, Source: "test"}, nil))

	client := connectInMemory(t, s)

	res, err := client.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name: "tether_tool_call",
		Arguments: map[string]any{
			"name": "knowledge_write",
			"arguments": map[string]any{
				"key": "contract_note",
			},
		},
	})
	if err != nil {
		t.Fatalf("CallTool tether_tool_call: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected tool error from tether_tool_call: %v", res)
	}

	if gotMeta == nil {
		t.Fatal("expected _meta on forwarded call from tether_tool_call, got nil")
	}
	env := ExtractProvenanceMeta(gotMeta)
	if env == nil {
		t.Fatalf("missing tether.provenance in _meta from tether_tool_call: %v", gotMeta)
	}
	if env.SchemaVersion != ProvenanceSchemaVersion || env.SessionID != "sess-mc" || env.WorkstreamID != "ws-mc" {
		t.Errorf("unexpected provenance envelope: %+v", env)
	}
	if gotArgs["key"] != "contract_note" {
		t.Errorf("forwarded arguments altered: %v", gotArgs)
	}
}

// TestProvenance_ReplacesClientSuppliedStampWhenSessionConfigured verifies that any
// client-invented stamp is overwritten with authentic Tether provenance.
func TestProvenance_ReplacesClientSuppliedStampWhenSessionConfigured(t *testing.T) {
	var gotMeta map[string]any

	mc := &mockClient{
		callToolFunc: func(_ context.Context, params *mcpsdk.CallToolParams) (*mcpsdk.CallToolResult, error) {
			gotMeta = map[string]any(params.Meta)
			return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "ok"}}}, nil
		},
	}

	reg := NewToolRegistry()
	reg.Register("upstream", mc, []*mcpsdk.Tool{makeTool("some_tool")})

	router := NewProxyRouter(reg)

	ctx := callcontext.WithSnapshot(context.Background(), callcontext.Snapshot{Verified: true, SessionID: "sess-real", WorkstreamID: "ws-real"})
	call := ToolCall{
		ToolName: "some_tool",
		Meta: map[string]any{
			ProvenanceMetaKey: map[string]any{
				"schema_version": 999,
				"session_id":     "sess-spoofed",
				"workstream_id":  "ws-spoofed",
				"verified":       true,
			},
		},
	}

	_, err := router.Handle(ctx, call)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	env := ExtractProvenanceMeta(gotMeta)
	if env == nil {
		t.Fatal("missing provenance envelope")
	}
	if env.SchemaVersion != ProvenanceSchemaVersion {
		t.Errorf("schema_version = %d, want 2", env.SchemaVersion)
	}
	if env.SessionID != "sess-real" {
		t.Errorf("session_id = %q, want %q", env.SessionID, "sess-real")
	}
	if env.WorkstreamID != "ws-real" {
		t.Errorf("workstream_id = %q, want %q", env.WorkstreamID, "ws-real")
	}
	// Verification belongs to the resolved snapshot, never the inbound map.
	provMap, _ := gotMeta[ProvenanceMetaKey].(map[string]any)
	if provMap["verified"] != true {
		t.Errorf("resolved verification missing: %v", provMap)
	}
}

// TestProvenance_StripsClientSuppliedStampWhenNoSessionConfigured verifies that when
// no session is configured (empty sessionID), any client-invented stamp is stripped
// and no spurious _meta is forwarded if _meta is otherwise empty.
func TestProvenance_StripsClientSuppliedStampWhenNoSessionConfigured(t *testing.T) {
	var gotMeta map[string]any

	mc := &mockClient{
		callToolFunc: func(_ context.Context, params *mcpsdk.CallToolParams) (*mcpsdk.CallToolResult, error) {
			gotMeta = map[string]any(params.Meta)
			return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "ok"}}}, nil
		},
	}

	reg := NewToolRegistry()
	reg.Register("upstream", mc, []*mcpsdk.Tool{makeTool("some_tool")})

	router := NewProxyRouter(reg)

	// Inbound call carrying a client-invented stamp and no session in ctx.
	call := ToolCall{
		ToolName: "some_tool",
		Meta: map[string]any{
			ProvenanceMetaKey: map[string]any{
				"schema_version": 1,
				"session_id":     "sess-spoofed",
			},
		},
	}

	_, err := router.Handle(context.Background(), call)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if gotMeta != nil {
		if _, hasProv := gotMeta[ProvenanceMetaKey]; hasProv {
			t.Errorf("tether.provenance was forwarded when no session was configured: %v", gotMeta)
		}
		if len(gotMeta) == 0 {
			t.Errorf("manufactured empty _meta when provenance was stripped: %+v", gotMeta)
		}
	}
}

// TestProvenance_ReassignmentSnapshotPerCall verifies that if a session's workstream
// changes mid-session, each forwarding call resolves the latest snapshot rather than caching.
func TestProvenance_ReassignmentSnapshotPerCall(t *testing.T) {
	var gotMeta map[string]any

	mc := &mockClient{
		callToolFunc: func(_ context.Context, params *mcpsdk.CallToolParams) (*mcpsdk.CallToolResult, error) {
			gotMeta = map[string]any(params.Meta)
			return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "ok"}}}, nil
		},
	}

	reg := NewToolRegistry()
	reg.Register("upstream", mc, []*mcpsdk.Tool{makeTool("some_tool")})

	router := NewProxyRouter(reg)

	ctx := callcontext.WithSnapshot(context.Background(), callcontext.Snapshot{Verified: true, SessionID: "sess-live", WorkstreamID: "ws-first"})

	// Call 1: initial assignment.
	_, err := router.Handle(ctx, ToolCall{ToolName: "some_tool"})
	if err != nil {
		t.Fatalf("call 1: %v", err)
	}
	env1 := ExtractProvenanceMeta(gotMeta)
	if env1.WorkstreamID != "ws-first" {
		t.Errorf("call 1: workstream_id = %q, want %q", env1.WorkstreamID, "ws-first")
	}

	// Reassignment happens mid-session.
	ctx = callcontext.WithSnapshot(context.Background(), callcontext.Snapshot{Verified: true, SessionID: "sess-live", WorkstreamID: "ws-second"})

	// Call 2: reflects new snapshot.
	_, err = router.Handle(ctx, ToolCall{ToolName: "some_tool"})
	if err != nil {
		t.Fatalf("call 2: %v", err)
	}
	env2 := ExtractProvenanceMeta(gotMeta)
	if env2.WorkstreamID != "ws-second" {
		t.Errorf("call 2: workstream_id = %q, want %q", env2.WorkstreamID, "ws-second")
	}
}

// TestProvenance_SessionWithoutWorkstreamOmitsWorkstreamID verifies that a session
// with no assigned workstream stamps schema_version and session_id but omits workstream_id.
func TestProvenance_SessionWithoutWorkstreamOmitsWorkstreamID(t *testing.T) {
	var gotMeta map[string]any

	mc := &mockClient{
		callToolFunc: func(_ context.Context, params *mcpsdk.CallToolParams) (*mcpsdk.CallToolResult, error) {
			gotMeta = map[string]any(params.Meta)
			return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "ok"}}}, nil
		},
	}

	reg := NewToolRegistry()
	reg.Register("upstream", mc, []*mcpsdk.Tool{makeTool("some_tool")})

	router := NewProxyRouter(reg)

	ctx := callcontext.WithSnapshot(context.Background(), callcontext.Snapshot{Verified: true, SessionID: "sess-noworkstream", WorkstreamID: ""})
	_, err := router.Handle(ctx, ToolCall{ToolName: "some_tool"})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	env := ExtractProvenanceMeta(gotMeta)
	if env == nil {
		t.Fatal("missing provenance envelope")
	}
	if env.SessionID != "sess-noworkstream" {
		t.Errorf("session_id = %q, want %q", env.SessionID, "sess-noworkstream")
	}
	if env.WorkstreamID != "" {
		t.Errorf("workstream_id = %q, want empty", env.WorkstreamID)
	}

	provMap, ok := gotMeta[ProvenanceMetaKey].(map[string]any)
	if !ok {
		t.Fatalf("expected map[string]any in _meta: %v", gotMeta[ProvenanceMetaKey])
	}
	if _, ok := provMap["workstream_id"]; ok {
		t.Errorf("workstream_id key should be omitted when empty: %v", provMap)
	}
}

// TestProvenance_FailedSessionLookupOmitsEnvelope verifies that if
// session lookup fails (e.g. database error), a bounded warning is logged, the
// envelope is omitted, any client-supplied stamp is stripped, and the content call
// is NOT failed.
func TestProvenance_FailedSessionLookupOmitsEnvelope(t *testing.T) {
	var gotMeta map[string]any
	var executed bool

	mc := &mockClient{
		callToolFunc: func(_ context.Context, params *mcpsdk.CallToolParams) (*mcpsdk.CallToolResult, error) {
			executed = true
			gotMeta = map[string]any(params.Meta)
			return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "ok"}}}, nil
		},
	}

	reg := NewToolRegistry()
	reg.Register("upstream", mc, []*mcpsdk.Tool{makeTool("content_tool")})

	router := NewProxyRouter(reg)

	a := New(nil, "", nil)
	a.SessionID = "sess-err"
	a.SetCallerContextResolver(func(context.Context) (callcontext.Snapshot, error) {
		return callcontext.Snapshot{}, errors.New("lookup failed")
	})
	ctx := a.withSessionID(context.Background())
	call := ToolCall{
		ToolName: "content_tool",
		Meta: map[string]any{
			ProvenanceMetaKey: "client-stamp-to-strip",
		},
	}

	res, err := router.Handle(ctx, call)
	if err != nil {
		t.Fatalf("tool call should not fail on lookup error: %v", err)
	}
	if res.IsError {
		t.Fatalf("tool result should not indicate error on lookup failure: %v", res)
	}
	if !executed {
		t.Fatal("content tool was not executed")
	}

	// Envelope must be omitted, and client stamp stripped.
	if gotMeta != nil {
		if _, hasProv := gotMeta[ProvenanceMetaKey]; hasProv {
			t.Errorf("tether.provenance was not stripped after lookup failure: %v", gotMeta)
		}
	}
}

// TestProvenance_PreservesUnrelatedMetaAndTraceContext verifies that progress tokens,
// custom metadata fields, and OpenTelemetry trace context are all preserved.
func TestProvenance_PreservesUnrelatedMetaAndTraceContext(t *testing.T) {
	var gotMeta map[string]any
	var gotArgs map[string]any

	mc := &mockClient{
		callToolFunc: func(_ context.Context, params *mcpsdk.CallToolParams) (*mcpsdk.CallToolResult, error) {
			gotMeta = map[string]any(params.Meta)
			gotArgs, _ = params.Arguments.(map[string]any)
			return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "ok"}}}, nil
		},
	}

	reg := NewToolRegistry()
	reg.Register("upstream", mc, []*mcpsdk.Tool{makeTool("meta_test_tool")})

	router := NewProxyRouter(reg)

	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3},
		SpanID:     trace.SpanID{4, 4, 4, 4, 4, 4, 4, 4},
		TraceFlags: trace.FlagsSampled,
	})
	ctx := trace.ContextWithSpanContext(context.Background(), sc)
	ctx = callcontext.WithSnapshot(ctx, callcontext.Snapshot{Verified: true, SessionID: "sess-preserve", WorkstreamID: "ws-preserve"})

	call := ToolCall{
		ToolName: "meta_test_tool",
		Args: map[string]any{
			"payload": "value",
		},
		Meta: map[string]any{
			"progressToken":       "progress-12345",
			"custom_client_field": "keep-me",
		},
	}

	_, err := router.Handle(ctx, call)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if gotMeta == nil {
		t.Fatal("expected _meta on forwarded call")
	}

	// 1. ProgressToken preserved
	if gotMeta["progressToken"] != "progress-12345" {
		t.Errorf("progressToken = %v, want progress-12345", gotMeta["progressToken"])
	}

	// 2. Custom field preserved
	if gotMeta["custom_client_field"] != "keep-me" {
		t.Errorf("custom_client_field altered: %v", gotMeta["custom_client_field"])
	}

	// 3. Provenance stamped
	env := ExtractProvenanceMeta(gotMeta)
	if env == nil || env.SessionID != "sess-preserve" || env.WorkstreamID != "ws-preserve" {
		t.Errorf("provenance envelope incorrect: %+v", env)
	}

	// 4. Trace context injected
	if tp, ok := gotMeta["_traceparent"].(string); !ok || tp == "" {
		t.Errorf("expected _traceparent in _meta, got: %v", gotMeta)
	}

	// 5. Arguments untouched
	if gotArgs["payload"] != "value" {
		t.Errorf("arguments altered: %v", gotArgs)
	}
}
