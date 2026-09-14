package mcpadapter

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"go.opentelemetry.io/otel/trace"
)

// TestProvenance_DirectProxiedCallStampsEnvelope tests that a direct proxied tool call
// stamps tether.provenance into params._meta with schema_version, session_id, and workstream_id.
func TestProvenance_DirectProxiedCallStampsEnvelope(t *testing.T) {
	var gotMeta *mcp.Meta
	var gotArgs map[string]any

	mc := &mockClient{
		callToolFunc: func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			gotMeta = req.Params.Meta
			gotArgs = req.GetArguments()
			return mcp.NewToolResultText("ok"), nil
		},
	}

	reg := NewToolRegistry()
	reg.Register("tesseract", mc, []mcp.Tool{makeTool("workspace_write")})

	router := NewProxyRouter(reg)
	router.SetWorkstreamResolver(func(_ context.Context, sessionID string) (string, error) {
		if sessionID == "sess-100" {
			return "ws-200", nil
		}
		return "", nil
	})

	ctx := WithSessionID(context.Background(), "sess-100")
	req := mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      "workspace_write",
			Arguments: map[string]any{"item_id": "item-1", "summary": "draft"},
		},
	}

	res, err := router.Handle(ctx, req)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected tool error: %v", res)
	}

	if gotMeta == nil {
		t.Fatal("expected _meta on forwarded call, got nil")
	}
	env := ExtractProvenanceMeta(mcp.CallToolRequest{Params: mcp.CallToolParams{Meta: gotMeta}})
	if env == nil {
		t.Fatalf("missing tether.provenance in _meta: %v", gotMeta.AdditionalFields)
	}
	if env.SchemaVersion != 1 {
		t.Errorf("schema_version = %d, want 1", env.SchemaVersion)
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

// TestProvenance_MuxCallForwardingStampsEnvelope tests that forwarding through mux_call
// stamps tether.provenance onto the upstream request.
func TestProvenance_MuxCallForwardingStampsEnvelope(t *testing.T) {
	var gotMeta *mcp.Meta
	var gotArgs map[string]any

	mc := &mockClient{
		callToolFunc: func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			gotMeta = req.Params.Meta
			gotArgs = req.GetArguments()
			return mcp.NewToolResultText("ok"), nil
		},
	}

	reg := NewToolRegistry()
	reg.Register("tesseract", mc, []mcp.Tool{makeTool("knowledge_write")})

	router := NewProxyRouter(reg)
	router.SetWorkstreamResolver(func(_ context.Context, sessionID string) (string, error) {
		if sessionID == "sess-mc" {
			return "ws-mc", nil
		}
		return "", nil
	})

	a := New(nil, "", nil)
	a.SessionID = "sess-mc"

	s := mcpserver.NewMCPServer("test-mux", "0.0.1", mcpserver.WithToolCapabilities(true))
	a.registerCallTool(s, router)

	client, err := mcpclient.NewInProcessClient(s)
	if err != nil {
		t.Fatalf("NewInProcessClient: %v", err)
	}
	defer func() { _ = client.Close() }()
	if _, err := client.Initialize(context.Background(), mcp.InitializeRequest{}); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	callReq := mcp.CallToolRequest{}
	callReq.Params.Name = "mux_call"
	callReq.Params.Arguments = map[string]any{
		"tool_name": "knowledge_write",
		"arguments": map[string]any{
			"key": "contract_note",
		},
	}

	res, err := client.CallTool(context.Background(), callReq)
	if err != nil {
		t.Fatalf("CallTool mux_call: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected tool error from mux_call: %v", res)
	}

	if gotMeta == nil {
		t.Fatal("expected _meta on forwarded call from mux_call, got nil")
	}
	env := ExtractProvenanceMeta(mcp.CallToolRequest{Params: mcp.CallToolParams{Meta: gotMeta}})
	if env == nil {
		t.Fatalf("missing tether.provenance in _meta from mux_call: %v", gotMeta.AdditionalFields)
	}
	if env.SchemaVersion != 1 || env.SessionID != "sess-mc" || env.WorkstreamID != "ws-mc" {
		t.Errorf("unexpected provenance envelope: %+v", env)
	}
	if gotArgs["key"] != "contract_note" {
		t.Errorf("forwarded arguments altered: %v", gotArgs)
	}
}

// TestProvenance_ReplacesClientSuppliedStampWhenSessionConfigured verifies that any
// client-invented stamp is overwritten with authentic Tether provenance.
func TestProvenance_ReplacesClientSuppliedStampWhenSessionConfigured(t *testing.T) {
	var gotMeta *mcp.Meta

	mc := &mockClient{
		callToolFunc: func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			gotMeta = req.Params.Meta
			return mcp.NewToolResultText("ok"), nil
		},
	}

	reg := NewToolRegistry()
	reg.Register("upstream", mc, []mcp.Tool{makeTool("some_tool")})

	router := NewProxyRouter(reg)
	router.SetWorkstreamResolver(func(_ context.Context, _ string) (string, error) {
		return "ws-real", nil
	})

	ctx := WithSessionID(context.Background(), "sess-real")
	req := mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "some_tool",
			Meta: &mcp.Meta{
				AdditionalFields: map[string]any{
					ProvenanceMetaKey: map[string]any{
						"schema_version": 999,
						"session_id":     "sess-spoofed",
						"workstream_id":  "ws-spoofed",
						"verified":       true,
					},
				},
			},
		},
	}

	_, err := router.Handle(ctx, req)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	env := ExtractProvenanceMeta(mcp.CallToolRequest{Params: mcp.CallToolParams{Meta: gotMeta}})
	if env == nil {
		t.Fatal("missing provenance envelope")
	}
	if env.SchemaVersion != 1 {
		t.Errorf("schema_version = %d, want 1", env.SchemaVersion)
	}
	if env.SessionID != "sess-real" {
		t.Errorf("session_id = %q, want %q", env.SessionID, "sess-real")
	}
	if env.WorkstreamID != "ws-real" {
		t.Errorf("workstream_id = %q, want %q", env.WorkstreamID, "ws-real")
	}
	// Verify spoofed fields (like "verified") are not in the envelope map.
	provMap, _ := gotMeta.AdditionalFields[ProvenanceMetaKey].(map[string]any)
	if _, ok := provMap["verified"]; ok {
		t.Errorf("spoofed 'verified' field survived in provenance map: %v", provMap)
	}
}

// TestProvenance_StripsClientSuppliedStampWhenNoSessionConfigured verifies that when
// no session is configured (empty sessionID), any client-invented stamp is stripped
// and no spurious _meta is forwarded if _meta is otherwise empty.
func TestProvenance_StripsClientSuppliedStampWhenNoSessionConfigured(t *testing.T) {
	var gotMeta *mcp.Meta

	mc := &mockClient{
		callToolFunc: func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			gotMeta = req.Params.Meta
			return mcp.NewToolResultText("ok"), nil
		},
	}

	reg := NewToolRegistry()
	reg.Register("upstream", mc, []mcp.Tool{makeTool("some_tool")})

	router := NewProxyRouter(reg)

	// Inbound call carrying a client-invented stamp and no session in ctx.
	req := mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "some_tool",
			Meta: &mcp.Meta{
				AdditionalFields: map[string]any{
					ProvenanceMetaKey: map[string]any{
						"schema_version": 1,
						"session_id":     "sess-spoofed",
					},
				},
			},
		},
	}

	_, err := router.Handle(context.Background(), req)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if gotMeta != nil {
		if _, hasProv := gotMeta.AdditionalFields[ProvenanceMetaKey]; hasProv {
			t.Errorf("tether.provenance was forwarded when no session was configured: %v", gotMeta.AdditionalFields)
		}
		if len(gotMeta.AdditionalFields) == 0 && gotMeta.ProgressToken == nil {
			t.Errorf("manufactured empty _meta when provenance was stripped: %+v", gotMeta)
		}
	}
}

// TestProvenance_ReassignmentSnapshotPerCall verifies that if a session's workstream
// changes mid-session, each forwarding call resolves the latest snapshot rather than caching.
func TestProvenance_ReassignmentSnapshotPerCall(t *testing.T) {
	var gotMeta *mcp.Meta

	mc := &mockClient{
		callToolFunc: func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			gotMeta = req.Params.Meta
			return mcp.NewToolResultText("ok"), nil
		},
	}

	reg := NewToolRegistry()
	reg.Register("upstream", mc, []mcp.Tool{makeTool("some_tool")})

	router := NewProxyRouter(reg)

	var mu sync.Mutex
	currentWorkstream := "ws-first"

	router.SetWorkstreamResolver(func(_ context.Context, _ string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		return currentWorkstream, nil
	})

	ctx := WithSessionID(context.Background(), "sess-live")

	// Call 1: initial assignment.
	_, err := router.Handle(ctx, mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "some_tool"}})
	if err != nil {
		t.Fatalf("call 1: %v", err)
	}
	env1 := ExtractProvenanceMeta(mcp.CallToolRequest{Params: mcp.CallToolParams{Meta: gotMeta}})
	if env1.WorkstreamID != "ws-first" {
		t.Errorf("call 1: workstream_id = %q, want %q", env1.WorkstreamID, "ws-first")
	}

	// Reassignment happens mid-session.
	mu.Lock()
	currentWorkstream = "ws-second"
	mu.Unlock()

	// Call 2: reflects new snapshot.
	_, err = router.Handle(ctx, mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "some_tool"}})
	if err != nil {
		t.Fatalf("call 2: %v", err)
	}
	env2 := ExtractProvenanceMeta(mcp.CallToolRequest{Params: mcp.CallToolParams{Meta: gotMeta}})
	if env2.WorkstreamID != "ws-second" {
		t.Errorf("call 2: workstream_id = %q, want %q", env2.WorkstreamID, "ws-second")
	}
}

// TestProvenance_SessionWithoutWorkstreamOmitsWorkstreamID verifies that a session
// with no assigned workstream stamps schema_version and session_id but omits workstream_id.
func TestProvenance_SessionWithoutWorkstreamOmitsWorkstreamID(t *testing.T) {
	var gotMeta *mcp.Meta

	mc := &mockClient{
		callToolFunc: func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			gotMeta = req.Params.Meta
			return mcp.NewToolResultText("ok"), nil
		},
	}

	reg := NewToolRegistry()
	reg.Register("upstream", mc, []mcp.Tool{makeTool("some_tool")})

	router := NewProxyRouter(reg)
	router.SetWorkstreamResolver(func(_ context.Context, _ string) (string, error) {
		return "", nil // no workstream assigned
	})

	ctx := WithSessionID(context.Background(), "sess-noworkstream")
	_, err := router.Handle(ctx, mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "some_tool"}})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	env := ExtractProvenanceMeta(mcp.CallToolRequest{Params: mcp.CallToolParams{Meta: gotMeta}})
	if env == nil {
		t.Fatal("missing provenance envelope")
	}
	if env.SessionID != "sess-noworkstream" {
		t.Errorf("session_id = %q, want %q", env.SessionID, "sess-noworkstream")
	}
	if env.WorkstreamID != "" {
		t.Errorf("workstream_id = %q, want empty", env.WorkstreamID)
	}

	provMap, ok := gotMeta.AdditionalFields[ProvenanceMetaKey].(map[string]any)
	if !ok {
		t.Fatalf("expected map[string]any in _meta: %v", gotMeta.AdditionalFields[ProvenanceMetaKey])
	}
	if _, ok := provMap["workstream_id"]; ok {
		t.Errorf("workstream_id key should be omitted when empty: %v", provMap)
	}
}

// TestProvenance_FailedSessionLookupLogsWarningAndOmitsEnvelope verifies that if
// session lookup fails (e.g. database error), a bounded warning is logged, the
// envelope is omitted, any client-supplied stamp is stripped, and the content call
// is NOT failed.
func TestProvenance_FailedSessionLookupLogsWarningAndOmitsEnvelope(t *testing.T) {
	var gotMeta *mcp.Meta
	var executed bool

	mc := &mockClient{
		callToolFunc: func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			executed = true
			gotMeta = req.Params.Meta
			return mcp.NewToolResultText("ok"), nil
		},
	}

	reg := NewToolRegistry()
	reg.Register("upstream", mc, []mcp.Tool{makeTool("content_tool")})

	router := NewProxyRouter(reg)

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	router.SetLogger(logger)

	router.SetWorkstreamResolver(func(_ context.Context, _ string) (string, error) {
		return "", errors.New("db locked or session not found")
	})

	ctx := WithSessionID(context.Background(), "sess-err")
	req := mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "content_tool",
			Meta: &mcp.Meta{
				AdditionalFields: map[string]any{
					ProvenanceMetaKey: "client-stamp-to-strip",
				},
			},
		},
	}

	res, err := router.Handle(ctx, req)
	if err != nil {
		t.Fatalf("tool call should not fail on lookup error: %v", err)
	}
	if res.IsError {
		t.Fatalf("tool result should not indicate error on lookup failure: %v", res)
	}
	if !executed {
		t.Fatal("content tool was not executed")
	}

	// Warning must be logged.
	if !bytes.Contains(logBuf.Bytes(), []byte("failed to resolve session workstream for provenance")) {
		t.Errorf("expected warning log, got: %s", logBuf.String())
	}

	// Envelope must be omitted, and client stamp stripped.
	if gotMeta != nil {
		if _, hasProv := gotMeta.AdditionalFields[ProvenanceMetaKey]; hasProv {
			t.Errorf("tether.provenance was not stripped after lookup failure: %v", gotMeta.AdditionalFields)
		}
	}
}

// TestProvenance_PreservesUnrelatedMetaAndTraceContext verifies that progress tokens,
// custom metadata fields, and OpenTelemetry trace context are all preserved.
func TestProvenance_PreservesUnrelatedMetaAndTraceContext(t *testing.T) {
	var gotMeta *mcp.Meta
	var gotArgs map[string]any

	mc := &mockClient{
		callToolFunc: func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			gotMeta = req.Params.Meta
			gotArgs = req.GetArguments()
			return mcp.NewToolResultText("ok"), nil
		},
	}

	reg := NewToolRegistry()
	reg.Register("upstream", mc, []mcp.Tool{makeTool("meta_test_tool")})

	router := NewProxyRouter(reg)
	router.SetWorkstreamResolver(func(_ context.Context, _ string) (string, error) {
		return "ws-preserve", nil
	})

	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 3},
		SpanID:     trace.SpanID{4, 4, 4, 4, 4, 4, 4, 4},
		TraceFlags: trace.FlagsSampled,
	})
	ctx := trace.ContextWithSpanContext(context.Background(), sc)
	ctx = WithSessionID(ctx, "sess-preserve")

	req := mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "meta_test_tool",
			Meta: &mcp.Meta{
				ProgressToken: "progress-12345",
				AdditionalFields: map[string]any{
					"custom_client_field": "keep-me",
				},
			},
			Arguments: map[string]any{
				"payload": "value",
			},
		},
	}

	_, err := router.Handle(ctx, req)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if gotMeta == nil {
		t.Fatal("expected _meta on forwarded call")
	}

	// 1. ProgressToken preserved
	if gotMeta.ProgressToken != "progress-12345" {
		t.Errorf("ProgressToken = %v, want progress-12345", gotMeta.ProgressToken)
	}

	// 2. Custom field preserved
	if gotMeta.AdditionalFields["custom_client_field"] != "keep-me" {
		t.Errorf("custom_client_field altered: %v", gotMeta.AdditionalFields["custom_client_field"])
	}

	// 3. Provenance stamped
	env := ExtractProvenanceMeta(mcp.CallToolRequest{Params: mcp.CallToolParams{Meta: gotMeta}})
	if env == nil || env.SessionID != "sess-preserve" || env.WorkstreamID != "ws-preserve" {
		t.Errorf("provenance envelope incorrect: %+v", env)
	}

	// 4. Trace context injected
	if tp, ok := gotMeta.AdditionalFields["_traceparent"].(string); !ok || tp == "" {
		t.Errorf("expected _traceparent in _meta, got: %v", gotMeta.AdditionalFields)
	}

	// 5. Arguments untouched
	if gotArgs["payload"] != "value" {
		t.Errorf("arguments altered: %v", gotArgs)
	}
}
