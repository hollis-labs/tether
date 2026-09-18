package mcpadapter

// CW-20260912-0061 (S3), CW-20260914-0003 (Slice 2).

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	gomcp "github.com/hollis-labs/go-mcp/server"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type recordedRef struct{ sessionID, kind, refID, uri, relation, source, parentItemID string }

type recordingAttacher struct{ got []recordedRef }

func (r *recordingAttacher) AttachSessionRef(_ context.Context, sessionID, kind, refID, uri, relation, source, parentItemID string) error {
	r.got = append(r.got, recordedRef{sessionID, kind, refID, uri, relation, source, parentItemID})
	return nil
}

type mockRefResolver struct {
	resolveFunc func(ctx context.Context, selector map[string]any) (*ResolvedRef, error)
}

func (m *mockRefResolver) ResolveRef(ctx context.Context, selector map[string]any) (*ResolvedRef, error) {
	if m.resolveFunc != nil {
		return m.resolveFunc(ctx, selector)
	}
	return nil, errors.New("mock resolver: not configured")
}

func TestExtractRefs_MatchesTheAllowlistOnly(t *testing.T) {
	args := map[string]any{
		"id": "CW-20260912-0061",
		"to": "msg://agent/agent-mux/agt_7qrvwg79ad",
		// In Slice 2, arbitrary ULID shapes in arguments are NOT guessed as revisions:
		"revision": "01M2B9SRHB4VZ3NE8VFR91XVVF",
		// Everything below must be ignored — this is the privacy line.
		"body":     "a long prose payload that mentions nothing structured",
		"token":    "sk-live-abcdef0123456789",
		"password": "hunter2",
		"count":    42,
	}
	got := extractRefs("torque_task_get", args)
	if got.refused {
		t.Fatal("unexpected truncation")
	}
	kinds := map[string]string{}
	for _, r := range got.refs {
		kinds[r.Kind] = r.RefID
	}
	want := map[string]string{
		refKindTorqueTask:   "CW-20260912-0061",
		refKindMessagingURN: "msg://agent/agent-mux/agt_7qrvwg79ad",
	}
	if !reflect.DeepEqual(kinds, want) {
		t.Errorf("extracted %v, want %v", kinds, want)
	}
	// The secret-shaped values must not appear anywhere in the output. This
	// is the assertion the whole privacy argument rests on, so it is checked
	// against the serialized refs rather than by inspecting fields.
	blob, _ := json.Marshal(got.refs)
	for _, secret := range []string{"sk-live", "hunter2", "long prose", "01M2B9SRHB4VZ3NE8VFR91XVVF"} {
		if strings.Contains(string(blob), secret) {
			t.Errorf("non-matching argument value %q leaked into extracted refs: %s", secret, blob)
		}
	}
}

func TestExtractRefs_WalksNestedArgs(t *testing.T) {
	args := map[string]any{
		"filter": map[string]any{
			"tasks": []any{"CW-20260912-0061", "CW-20260912-0074"},
		},
	}
	got := extractRefs("torque_task_list", args)
	if len(got.refs) != 2 {
		t.Fatalf("got %d refs, want 2: %+v", len(got.refs), got.refs)
	}
	for _, r := range got.refs {
		if r.Relation != relationRead {
			t.Errorf("relation = %q, want read for a _list verb", r.Relation)
		}
	}
}

// A wide payload must yield NOTHING rather than a partial set. A partial
// extraction is a silently incomplete ref set — the digest looks answered
// rather than truncated, which is the failure hardest to notice later.
func TestExtractRefs_RefusesRatherThanTruncating(t *testing.T) {
	wide := map[string]any{}
	for i := 0; i < maxScanValues+50; i++ {
		wide[string(rune('a'+i%26))+string(rune('0'+i/26))] = "filler"
	}
	wide["real"] = "CW-20260912-0061"

	got := extractRefs("torque_task_get", wide)
	if !got.refused {
		t.Fatal("a payload past the value ceiling must report refusal")
	}
	if len(got.refs) != 0 {
		t.Errorf("got %d refs on a truncated scan; a partial set is worse than none because nothing marks it incomplete", len(got.refs))
	}
}

func TestExtractRefs_RefusesPathologicalDepth(t *testing.T) {
	var deep any = "CW-20260912-0061"
	for i := 0; i < maxScanDepth+5; i++ {
		deep = map[string]any{"n": deep}
	}
	got := extractRefs("torque_task_get", map[string]any{"root": deep})
	if !got.refused || len(got.refs) != 0 {
		t.Errorf("deep nesting must refuse: refused=%v refs=%d", got.refused, len(got.refs))
	}
}

func TestRelationForTool(t *testing.T) {
	cases := map[string]string{
		"torque_task_create":     relationCreated,
		"memory_write":           relationCreated,
		"torque_task_update":     relationUpdated,
		"torque_task_transition": relationUpdated,
		"torque_task_get":        relationRead,
		"something_unheard_of":   relationReferenced,
	}
	for tool, want := range cases {
		if got := relationForTool(tool); got != want {
			t.Errorf("relationForTool(%q) = %q, want %q", tool, got, want)
		}
	}
}

// Duplicate identifiers in one call collapse to one ref.
func TestExtractRefs_Deduplicates(t *testing.T) {
	got := extractRefs("torque_task_get", map[string]any{
		"a": "CW-20260912-0061",
		"b": "CW-20260912-0061",
	})
	if len(got.refs) != 1 {
		t.Errorf("got %d refs, want 1", len(got.refs))
	}
}

// ── the proxied path, end to end ───────────────────────────────────────────

// runProxiedCall drives a real in-process MCP client through the handler
// addProxyTools registered, so extraction is exercised where it actually
// hangs rather than through a reconstruction of the wiring.
func runProxiedCall(t *testing.T, a *Adapter, args map[string]any, upstreamErrors bool) {
	runProxiedToolCall(t, a, "torque_task_get", args, `{"ok": true}`, upstreamErrors)
}

func runProxiedToolCall(t *testing.T, a *Adapter, toolName string, args map[string]any, responseText string, upstreamErrors bool) {
	t.Helper()

	mc := &mockClient{
		callToolFunc: func(context.Context, *mcpsdk.CallToolParams) (*mcpsdk.CallToolResult, error) {
			if upstreamErrors {
				return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "upstream said no"}}, IsError: true}, nil
			}
			return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: responseText}}}, nil
		},
	}
	reg := NewToolRegistry()
	reg.Register("upstream", mc, []*mcpsdk.Tool{makeTool(toolName)})

	s := gomcp.NewServer("t", "0.0.1")
	idx := NewDiscoveryIndex()
	idx.Build(reg, nil)
	live := &liveProxyCatalog{
		adapter: a, server: s, registry: reg,
		router: NewProxyRouter(reg), index: idx, firehose: true,
	}
	live.addProxyTools(makeTool(toolName))

	c := connectInMemory(t, s)
	ctx := context.Background()
	if _, err := c.CallTool(ctx, &mcpsdk.CallToolParams{Name: toolName, Arguments: args}); err != nil {
		t.Fatalf("CallTool: %v", err)
	}
}

func newExtractingAdapter(sessionID string, enabled bool) (*Adapter, *recordingAttacher) {
	attacher := &recordingAttacher{}
	a := &Adapter{SessionID: sessionID, ExtractRefs: enabled}
	a.SetRefAttacher(attacher)
	return a, attacher
}

// The acceptance criterion: a session calling torque_task_get on a real id
// produces a torque_task ref with relation=read and source=proxy.
func TestProxiedCall_ExtractsRefWithProxySource(t *testing.T) {
	a, attacher := newExtractingAdapter("sess-1", true)
	runProxiedCall(t, a, map[string]any{"id": "CW-20260912-0061"}, false)

	if len(attacher.got) != 1 {
		t.Fatalf("got %d refs, want 1: %+v", len(attacher.got), attacher.got)
	}
	ref := attacher.got[0]
	if ref.sessionID != "sess-1" || ref.kind != refKindTorqueTask ||
		ref.refID != "CW-20260912-0061" || ref.relation != relationRead || ref.source != "proxy" {
		t.Errorf("ref = %+v", ref)
	}
}

// A failed call leaves no ref. session_refs has no column to say a call
// errored — the triple is closed — so recording relation=read for a call that
// did not happen would assert something false.
func TestProxiedCall_FailedCallLeavesNoRef(t *testing.T) {
	a, attacher := newExtractingAdapter("sess-1", true)
	runProxiedCall(t, a, map[string]any{"id": "CW-20260912-0061"}, true)

	if len(attacher.got) != 0 {
		t.Errorf("a failed call produced %d refs; it touched nothing", len(attacher.got))
	}
}

// Off by default. This captures argument/response VALUES rather than shapes,
// so it stays opt-in until watched on real traffic.
func TestProxiedCall_DisabledByDefault(t *testing.T) {
	a, attacher := newExtractingAdapter("sess-1", false)
	runProxiedCall(t, a, map[string]any{"id": "CW-20260912-0061"}, false)

	if len(attacher.got) != 0 {
		t.Errorf("extraction ran while disabled: %+v", attacher.got)
	}
}

// No session, no refs. A hand-launched client reaches the proxy with no
// Tether session; an extracted ref has nothing to attach to, and a fabricated
// attribution would be worse than an absent one.
func TestProxiedCall_NoSessionMeansNoRefs(t *testing.T) {
	a, attacher := newExtractingAdapter("", true)
	runProxiedCall(t, a, map[string]any{"id": "CW-20260912-0061"}, false)

	if len(attacher.got) != 0 {
		t.Errorf("extraction ran with no session: %+v", attacher.got)
	}
}

// CW-20260914-0003: A response naming item_id (no revision_id) is captured as
// tesseract_item, not guessed as a revision.
func TestProxiedCall_ExtractsItemFromResponse(t *testing.T) {
	a, attacher := newExtractingAdapter("sess-1", true)
	resp := `{"status":"created","item_id":"01M2ITEM000000000000000000","version_token":"v1"}`
	runProxiedToolCall(t, a, "workspace_write", map[string]any{"summary": "draft"}, resp, false)

	if len(attacher.got) != 1 {
		t.Fatalf("got %d refs, want 1: %+v", len(attacher.got), attacher.got)
	}
	ref := attacher.got[0]
	if ref.kind != refKindTesseractItem {
		t.Errorf("kind = %q, want %q", ref.kind, refKindTesseractItem)
	}
	if ref.refID != "01M2ITEM000000000000000000" {
		t.Errorf("refID = %q, want %q", ref.refID, "01M2ITEM000000000000000000")
	}
	if ref.relation != relationCreated {
		t.Errorf("relation = %q, want %q", ref.relation, relationCreated)
	}
	if ref.uri != "tesseract://item/01M2ITEM000000000000000000" {
		t.Errorf("uri = %q, want tesseract://item/...", ref.uri)
	}
}

// CW-20260914-0003: A response naming an exact revision_id is captured as
// tesseract_revision with its parent item_id available as derived evidence.
func TestProxiedCall_ExtractsRevisionFromResponse(t *testing.T) {
	a, attacher := newExtractingAdapter("sess-1", true)
	resp := `{"status":"created","revision_id":"01M2REV0000000000000000000","item_id":"01M2ITEM000000000000000000"}`
	runProxiedToolCall(t, a, "memory_write", map[string]any{"summary": "saved note"}, resp, false)

	if len(attacher.got) != 1 {
		t.Fatalf("got %d refs, want 1: %+v", len(attacher.got), attacher.got)
	}
	ref := attacher.got[0]
	if ref.kind != refKindTesseractRevision {
		t.Errorf("kind = %q, want %q", ref.kind, refKindTesseractRevision)
	}
	if ref.refID != "01M2REV0000000000000000000" {
		t.Errorf("refID = %q, want %q", ref.refID, "01M2REV0000000000000000000")
	}
	if ref.relation != relationCreated {
		t.Errorf("relation = %q, want %q", ref.relation, relationCreated)
	}
	if ref.uri != "tesseract://revision/01M2REV0000000000000000000" {
		t.Errorf("uri = %q, want tesseract://revision/...", ref.uri)
	}
	if ref.parentItemID != "01M2ITEM000000000000000000" {
		t.Errorf("parentItemID = %q, want 01M2ITEM000000000000000000", ref.parentItemID)
	}
}

// CW-20260914-0003: workspace_write returning status:"replayed" produces no
// new creation claim in session_refs.
func TestProxiedCall_WorkspaceWriteReplay_ProducesNoCreationClaim(t *testing.T) {
	a, attacher := newExtractingAdapter("sess-1", true)
	resp := `{"status":"replayed","item_id":"01M2ITEM000000000000000000","availability":"live"}`
	runProxiedToolCall(t, a, "workspace_write", map[string]any{"summary": "draft", "idempotency_key": "k1"}, resp, false)

	if len(attacher.got) != 0 {
		t.Errorf("workspace_write replayed must produce no creation ref, got: %+v", attacher.got)
	}
}

// CW-20260914-0003: A key-only capture (no typed ID in response) is resolved
// via tesseract_ref_resolve at capture time and bound to the returned typed ref.
func TestProxiedCall_KeyOnlyCapture_ResolverBinding(t *testing.T) {
	a, attacher := newExtractingAdapter("sess-1", true)
	a.SetRefResolver(&mockRefResolver{
		resolveFunc: func(_ context.Context, selector map[string]any) (*ResolvedRef, error) {
			if selector["namespace"] == "project/foo/knowledge" && selector["key"] == "overview" {
				var res ResolvedRef
				res.Status = "resolved"
				res.Ref.Kind = refKindTesseractItem
				res.Ref.RefID = "01M2BOUND0000000000000000"
				res.Ref.URI = "tesseract://item/01M2BOUND0000000000000000"
				res.ItemID = "01M2BOUND0000000000000000"
				return &res, nil
			}
			return nil, errors.New("not found")
		},
	})

	// Upstream returns generic content with no item_id or revision_id
	resp := `{"body":"Architecture overview"}`
	runProxiedToolCall(t, a, "knowledge_get", map[string]any{
		"namespace": "project/foo/knowledge",
		"key":       "overview",
	}, resp, false)

	if len(attacher.got) != 1 {
		t.Fatalf("got %d refs, want 1: %+v", len(attacher.got), attacher.got)
	}
	ref := attacher.got[0]
	if ref.kind != refKindTesseractItem || ref.refID != "01M2BOUND0000000000000000" {
		t.Errorf("key capture was not bound to resolver typed ref: %+v", ref)
	}
	if ref.relation != relationRead {
		t.Errorf("relation = %q, want %q", ref.relation, relationRead)
	}
	if ref.parentItemID != "01M2BOUND0000000000000000" {
		t.Errorf("parentItemID = %q, want 01M2BOUND0000000000000000", ref.parentItemID)
	}
}

// CW-20260914-0003: Resolver failure must not turn an already-successful content
// operation into a failure: keep the unresolved capture with a diagnostic.
func TestProxiedCall_KeyOnlyCapture_ResolverFailure_PreservesUnresolvedCapture(t *testing.T) {
	a, attacher := newExtractingAdapter("sess-1", true)
	a.SetRefResolver(&mockRefResolver{
		resolveFunc: func(_ context.Context, _ map[string]any) (*ResolvedRef, error) {
			return nil, errors.New("resolver network timeout")
		},
	})

	resp := `{"body":"Some body"}`
	runProxiedToolCall(t, a, "knowledge_get", map[string]any{
		"namespace": "project/foo/knowledge",
		"key":       "unresolved_doc",
	}, resp, false)

	if len(attacher.got) != 1 {
		t.Fatalf("got %d refs, want 1: %+v", len(attacher.got), attacher.got)
	}
	ref := attacher.got[0]
	if ref.kind != refKindTesseractKey || ref.refID != "project/foo/knowledge:unresolved_doc" {
		t.Errorf("unexpected ref for failed resolver: %+v, want preserved tesseract_key", ref)
	}
	if ref.relation != relationRead {
		t.Errorf("relation = %q, want %q", ref.relation, relationRead)
	}
}

// CW-20260914-0003: Resolver / search previews (tesseract_recall, tesseract_ref_resolve)
// never count as content use and must NOT be attributed as read!
func TestProxiedCall_RecallPreviews_NeverCountAsRead(t *testing.T) {
	a, attacher := newExtractingAdapter("sess-1", true)
	recallResp := `{"results":[{"revision":{"revision_id":"01M2REV0000000000000000000","item_id":"01M2ITEM000000000000000000"}}]}`
	runProxiedToolCall(t, a, "tesseract_recall", map[string]any{"query": "search query"}, recallResp, false)

	if len(attacher.got) != 0 {
		t.Errorf("tesseract_recall previews must not be attributed as read, got: %+v", attacher.got)
	}

	resolveResp := `{"status":"resolved","ref":{"kind":"tesseract_item","ref_id":"01M2ITEM000000000000000000"}}`
	runProxiedToolCall(t, a, "tesseract_ref_resolve", map[string]any{"item_id": "01M2ITEM000000000000000000"}, resolveResp, false)

	if len(attacher.got) != 0 {
		t.Errorf("tesseract_ref_resolve previews must not be attributed as read, got: %+v", attacher.got)
	}
}

// CW-20260914-0003: mux_call forwards and extracts refs with source=proxy.
func TestProxiedCall_MuxCall_ExtractsRefs(t *testing.T) {
	a, attacher := newExtractingAdapter("sess-1", true)

	mc := &mockClient{
		callToolFunc: func(_ context.Context, _ *mcpsdk.CallToolParams) (*mcpsdk.CallToolResult, error) {
			return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: `{"status":"created","item_id":"01M2ITEM000000000000000000"}`}}}, nil
		},
	}
	reg := NewToolRegistry()
	reg.Register("upstream", mc, []*mcpsdk.Tool{makeTool("workspace_write")})

	s := gomcp.NewServer("t", "0.0.1")
	router := NewProxyRouter(reg)
	a.registerCallTool(s, router)

	c := connectInMemory(t, s)
	ctx := context.Background()
	if _, err := c.CallTool(ctx, &mcpsdk.CallToolParams{
		Name: "mux_call",
		Arguments: map[string]any{
			"tool_name": "workspace_write",
			"arguments": map[string]any{"summary": "dispatched draft"},
		},
	}); err != nil {
		t.Fatalf("CallTool mux_call: %v", err)
	}

	if len(attacher.got) != 1 {
		t.Fatalf("mux_call did not extract ref: %+v", attacher.got)
	}
	ref := attacher.got[0]
	if ref.kind != refKindTesseractItem || ref.refID != "01M2ITEM000000000000000000" || ref.source != "proxy" {
		t.Errorf("mux_call extracted unexpected ref: %+v", ref)
	}
}

// THE BOUNDARY. Extraction READS outbound arguments; it must never modify the
// forwarded call. Adding to the call is CW-20260912-0024 — the opposite
// direction through this same seam — and after four changes to this handler
// the muscle memory is real. Asserted rather than remembered.
func TestProxiedCall_DoesNotModifyTheForwardedRequest(t *testing.T) {
	args := map[string]any{"id": "CW-20260912-0061", "nested": map[string]any{"k": "v"}}
	before, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var forwarded []byte
	var forwardedMeta map[string]any
	mc := &mockClient{
		callToolFunc: func(_ context.Context, params *mcpsdk.CallToolParams) (*mcpsdk.CallToolResult, error) {
			forwardedArgs, _ := params.Arguments.(map[string]any)
			forwarded, _ = json.Marshal(forwardedArgs)
			forwardedMeta = map[string]any(params.Meta)
			return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "ok"}}}, nil
		},
	}
	reg := NewToolRegistry()
	reg.Register("torque", mc, []*mcpsdk.Tool{makeTool("torque_task_get")})

	a, _ := newExtractingAdapter("sess-1", true)
	s := gomcp.NewServer("t", "0.0.1")
	idx := NewDiscoveryIndex()
	idx.Build(reg, nil)
	live := &liveProxyCatalog{
		adapter: a, server: s, registry: reg,
		router: NewProxyRouter(reg), index: idx, firehose: true,
	}
	live.addProxyTools(makeTool("torque_task_get"))

	c := connectInMemory(t, s)
	if _, err := c.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: "torque_task_get", Arguments: args}); err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	if string(forwarded) != string(before) {
		t.Errorf("extraction altered the forwarded arguments:\n  before %s\n  after  %s\nThat is CW-20260912-0024's direction, not this task's.", before, forwarded)
	}
	// _meta may carry trace context (CW-20260907-0026) or provenance
	// (CW-20260913-0011) but nothing extraction-shaped.
	for k := range forwardedMeta {
		if k != "_traceparent" && k != "_tracestate" && k != ProvenanceMetaKey {
			t.Errorf("extraction added %q to _meta on the forwarded call", k)
		}
	}
}
