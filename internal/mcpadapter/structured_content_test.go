package mcpadapter

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestObjectStructuredContentWrapsNonObjects(t *testing.T) {
	text := []mcpsdk.Content{&mcpsdk.TextContent{Text: "kept"}}
	for name, in := range map[string]any{
		"array":       []any{map[string]any{"id": "d1"}},
		"empty array": []any{},
		"string":      "plain",
		"number":      float64(3),
		"bool":        true,
		"raw array":   json.RawMessage(`[1,2]`),
	} {
		orig := &mcpsdk.CallToolResult{Content: text, StructuredContent: in}
		got := objectStructuredContent(orig)
		m, ok := got.StructuredContent.(map[string]any)
		if !ok || len(m) != 1 || !reflect.DeepEqual(m["result"], in) {
			t.Errorf("%s: not wrapped as {result: value}: %#v", name, got.StructuredContent)
		}
		if len(got.Content) != 1 {
			t.Errorf("%s: text content lost", name)
		}
		if _, isMap := orig.StructuredContent.(map[string]any); isMap {
			t.Errorf("%s: the upstream result was mutated", name)
		}
	}
}

func TestObjectStructuredContentLeavesValidResultsAlone(t *testing.T) {
	for name, r := range map[string]*mcpsdk.CallToolResult{
		"nil result":   nil,
		"no content":   {},
		"object":       {StructuredContent: map[string]any{"a": 1}},
		"raw object":   {StructuredContent: json.RawMessage(` {"a":1}`)},
		"error result": {IsError: true},
	} {
		if got := objectStructuredContent(r); got != r {
			t.Errorf("%s: a valid result was copied or changed", name)
		}
	}
}

// End to end through the router every path (flat and tether_tool_call) uses: an upstream
// that returns a bare array (fragments-engine list_destinations) reaches the client as an object.
func TestProxyRouterWrapsArrayStructuredContent(t *testing.T) {
	reg := NewToolRegistry()
	upstream := &mockClient{callToolFunc: func(context.Context, *mcpsdk.CallToolParams) (*mcpsdk.CallToolResult, error) {
		return &mcpsdk.CallToolResult{
			Content:           []mcpsdk.Content{&mcpsdk.TextContent{Text: `[{"ID":"d1"}]`}},
			StructuredContent: []any{map[string]any{"ID": "d1"}},
		}, nil
	}}
	mustRegister(t, reg, "fragments-engine", upstream, []*mcpsdk.Tool{makeTool("list_destinations")})
	result, err := NewProxyRouter(reg).Handle(context.Background(), callReq("list_destinations"))
	if err != nil {
		t.Fatal(err)
	}
	wrapped, ok := result.StructuredContent.(map[string]any)
	if !ok || wrapped["result"] == nil {
		t.Fatalf("structuredContent still not an object: %#v", result.StructuredContent)
	}
	if text := result.Content[0].(*mcpsdk.TextContent).Text; text != `[{"ID":"d1"}]` {
		t.Fatalf("text content changed: %s", text)
	}
}
