package mcpadapter

// CW-20260912-0061 (S3).

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

type recordedRef struct{ sessionID, kind, refID, relation, source string }

type recordingAttacher struct{ got []recordedRef }

func (r *recordingAttacher) AttachSessionRef(_ context.Context, sessionID, kind, refID, relation, source string) error {
	r.got = append(r.got, recordedRef{sessionID, kind, refID, relation, source})
	return nil
}

func TestExtractRefs_MatchesTheAllowlistOnly(t *testing.T) {
	args := map[string]any{
		"id":       "CW-20260912-0061",
		"revision": "01M2B9SRHB4VZ3NE8VFR91XVVF",
		"to":       "msg://agent/agent-mux/agt_7qrvwg79ad",
		// Everything below must be ignored — this is the privacy line.
		"body":     "a long prose payload that mentions nothing structured",
		"token":    "sk-live-abcdef0123456789",
		"password": "hunter2",
		"count":    42,
	}
	got := extractRefs("torque_task_get", args)
	if got.truncated {
		t.Fatal("unexpected truncation")
	}
	kinds := map[string]string{}
	for _, r := range got.refs {
		kinds[r.Kind] = r.RefID
	}
	want := map[string]string{
		refKindTorqueTask:        "CW-20260912-0061",
		refKindTesseractRevision: "01M2B9SRHB4VZ3NE8VFR91XVVF",
		refKindMessagingURN:      "msg://agent/agent-mux/agt_7qrvwg79ad",
	}
	if !reflect.DeepEqual(kinds, want) {
		t.Errorf("extracted %v, want %v", kinds, want)
	}
	// The secret-shaped values must not appear anywhere in the output. This
	// is the assertion the whole privacy argument rests on, so it is checked
	// against the serialized refs rather than by inspecting fields.
	blob, _ := json.Marshal(got.refs)
	for _, secret := range []string{"sk-live", "hunter2", "long prose"} {
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
	if !got.truncated {
		t.Fatal("a payload past the value ceiling must report truncation")
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
	if !got.truncated || len(got.refs) != 0 {
		t.Errorf("deep nesting must refuse: truncated=%v refs=%d", got.truncated, len(got.refs))
	}
}

func TestRelationForTool(t *testing.T) {
	cases := map[string]string{
		"torque_task_create":     relationCreated,
		"memory_write":           relationCreated,
		"torque_task_update":     relationUpdated,
		"torque_task_transition": relationUpdated,
		"torque_task_get":        relationRead,
		"tesseract_recall":       relationRead,
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
	t.Helper()

	mc := &mockClient{
		callToolFunc: func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			if upstreamErrors {
				return mcp.NewToolResultError("upstream said no"), nil
			}
			return mcp.NewToolResultText("ok"), nil
		},
	}
	reg := NewToolRegistry()
	reg.Register("torque", mc, []mcp.Tool{makeTool("torque_task_get")})

	s := mcpserver.NewMCPServer("t", "0.0.1", mcpserver.WithToolCapabilities(true))
	idx := NewDiscoveryIndex()
	idx.Build(reg, nil)
	live := &liveProxyCatalog{
		adapter: a, server: s, registry: reg,
		router: NewProxyRouter(reg), index: idx, firehose: true,
	}
	live.addProxyTools(makeTool("torque_task_get"))

	c, err := mcpclient.NewInProcessClient(s)
	if err != nil {
		t.Fatalf("NewInProcessClient: %v", err)
	}
	defer func() { _ = c.Close() }()
	ctx := context.Background()
	if _, err := c.Initialize(ctx, mcp.InitializeRequest{}); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	req := mcp.CallToolRequest{}
	req.Params.Name = "torque_task_get"
	req.Params.Arguments = args
	if _, err := c.CallTool(ctx, req); err != nil {
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

// Off by default. This is the first thing in the proxy to capture argument
// VALUES rather than shapes, so it stays opt-in until watched on real traffic.
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
	var forwardedMeta *mcp.Meta
	mc := &mockClient{
		callToolFunc: func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			forwarded, _ = json.Marshal(req.GetArguments())
			forwardedMeta = req.Params.Meta
			return mcp.NewToolResultText("ok"), nil
		},
	}
	reg := NewToolRegistry()
	reg.Register("torque", mc, []mcp.Tool{makeTool("torque_task_get")})

	a, _ := newExtractingAdapter("sess-1", true)
	s := mcpserver.NewMCPServer("t", "0.0.1", mcpserver.WithToolCapabilities(true))
	idx := NewDiscoveryIndex()
	idx.Build(reg, nil)
	live := &liveProxyCatalog{
		adapter: a, server: s, registry: reg,
		router: NewProxyRouter(reg), index: idx, firehose: true,
	}
	live.addProxyTools(makeTool("torque_task_get"))

	c, err := mcpclient.NewInProcessClient(s)
	if err != nil {
		t.Fatalf("NewInProcessClient: %v", err)
	}
	defer func() { _ = c.Close() }()
	if _, err := c.Initialize(context.Background(), mcp.InitializeRequest{}); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	req := mcp.CallToolRequest{}
	req.Params.Name = "torque_task_get"
	req.Params.Arguments = args
	if _, err := c.CallTool(context.Background(), req); err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	if string(forwarded) != string(before) {
		t.Errorf("extraction altered the forwarded arguments:\n  before %s\n  after  %s\nThat is CW-20260912-0024's direction, not this task's.", before, forwarded)
	}
	// _meta may carry trace context (CW-20260907-0026) but nothing
	// extraction-shaped.
	if forwardedMeta != nil {
		for k := range forwardedMeta.AdditionalFields {
			if k != "_traceparent" && k != "_tracestate" {
				t.Errorf("extraction added %q to _meta on the forwarded call", k)
			}
		}
	}
}
