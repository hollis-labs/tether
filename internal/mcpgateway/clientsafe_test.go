package mcpgateway

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// caster_ls shape: Go nil slices serialized as JSON null.
func nullRequiredSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": nil,
		"properties": map[string]any{
			"path": map[string]any{"type": "string", "default": nil},
			"opts": map[string]any{"type": "object", "required": nil, "properties": nil},
			"list": map[string]any{"type": "array", "items": map[string]any{"type": "object", "required": nil}},
		},
		"anyOf": []any{map[string]any{"required": nil}},
	}
}

func TestClientSafeToolDropsNullKeywordsAndKeepsRealNulls(t *testing.T) {
	orig := &mcpsdk.Tool{Name: "caster_ls", InputSchema: nullRequiredSchema()}
	if problems := ValidateToolForClient(orig); len(problems) == 0 {
		t.Fatal("the validator accepts a schema with null required")
	}
	safe := ClientSafeTool(orig)
	if problems := ValidateToolForClient(safe); len(problems) != 0 {
		t.Fatalf("still invalid after cleaning: %v", problems)
	}
	if orig.InputSchema.(map[string]any)["required"] != nil || !hasKey(orig.InputSchema.(map[string]any), "required") {
		t.Fatal("the upstream definition was mutated")
	}
	got := safe.InputSchema.(map[string]any)
	path := got["properties"].(map[string]any)["path"].(map[string]any)
	if v, ok := path["default"]; !ok || v != nil {
		t.Fatalf("a null default is a value and must survive: %v", path)
	}
	raw, _ := json.Marshal(safe.InputSchema)
	if strings.Contains(string(raw), `"required":null`) || strings.Contains(string(raw), `"properties":null`) {
		t.Fatalf("null keyword left in %s", raw)
	}
}

func hasKey(m map[string]any, k string) bool { _, ok := m[k]; return ok }

func TestClientSafeToolReturnsCleanToolsUntouched(t *testing.T) {
	tool := &mcpsdk.Tool{Name: "ok", InputSchema: map[string]any{"type": "object", "required": []any{"a"}, "properties": map[string]any{"a": map[string]any{"type": "string"}}}}
	if got := ClientSafeTool(tool); got != tool {
		t.Fatal("a valid tool was copied")
	}
	if ClientSafeTool(nil) != nil {
		t.Fatal("nil tool")
	}
	raw := &mcpsdk.Tool{Name: "raw", InputSchema: json.RawMessage(`{"type":"object","required":null}`)}
	if problems := ValidateToolForClient(ClientSafeTool(raw)); len(problems) != 0 {
		t.Fatalf("raw schema: %v", problems)
	}
}

func TestValidateToolForClientReportsNamesAndShapes(t *testing.T) {
	cases := map[string]*mcpsdk.Tool{
		"dotted name":       {Name: "tangent.tesseract_review", InputSchema: map[string]any{"type": "object"}},
		"non-object root":   {Name: "x", InputSchema: map[string]any{"type": "string"}},
		"required not list": {Name: "x", InputSchema: map[string]any{"type": "object", "required": "a"}},
		"null property":     {Name: "x", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"a": nil}}},
	}
	for name, tool := range cases {
		if len(ValidateToolForClient(tool)) == 0 {
			t.Errorf("%s: not reported", name)
		}
	}
	if p := ValidateToolForClient(&mcpsdk.Tool{Name: "fine", InputSchema: map[string]any{"type": "object"}}); len(p) != 0 {
		t.Fatal(p)
	}
}

// The list a client actually receives: a null-required upstream tool must come out valid.
func TestProtocolListHandsClientsValidSchemas(t *testing.T) {
	bad := &mcpsdk.Tool{Name: "caster_ls", InputSchema: nullRequiredSchema()}
	good := &mcpsdk.Tool{Name: "caster_get", InputSchema: map[string]any{"type": "object"}}
	snapshot := Snapshot{Entries: []Entry{{Tool: bad, Origin: "caster"}, {Tool: good, Origin: "caster"}}, Origins: []OriginStatus{{ID: "caster", Status: "connected"}}}
	service := &Service{Selection: Selection{Mode: Flat}, Policy: &Policy{}, Snapshot: func() Snapshot { return snapshot }}
	listed, _, err := service.ProtocolList([]*mcpsdk.Tool{bad, good}, func(string) bool { return false }, "")
	if err != nil || len(listed) != 2 {
		t.Fatalf("listed=%d err=%v", len(listed), err)
	}
	for _, tool := range listed {
		if problems := ValidateToolForClient(tool); len(problems) != 0 {
			t.Errorf("%s: %v", tool.Name, problems)
		}
	}
	if !reflect.DeepEqual(bad.InputSchema, nullRequiredSchema()) {
		t.Fatal("registry definition changed")
	}
}

// The schemas rendered by tether_tool_list and tether_tool_search are clean too.
func TestListAndSearchRenderClientSafeSchemas(t *testing.T) {
	bad := &mcpsdk.Tool{Name: "caster_ls", Description: "List named items", InputSchema: nullRequiredSchema()}
	snapshot := Snapshot{Entries: []Entry{{Tool: bad, Origin: "caster"}}, Origins: []OriginStatus{{ID: "caster", Status: "connected"}}}
	service := &Service{Selection: Selection{Mode: Search}, Policy: &Policy{}, Snapshot: func() Snapshot { return snapshot }, Score: func(string, Entry) int { return 1 }}
	byNames, err := service.List(Request{Names: []string{"caster_ls"}})
	if err != nil {
		t.Fatal(err)
	}
	searched, err := service.Search(Request{Query: "list", Detail: "schema"})
	if err != nil {
		t.Fatal(err)
	}
	for name, result := range map[string]Result{"list": byNames, "search": searched} {
		if len(result.Items) != 1 {
			t.Fatalf("%s: %d items", name, len(result.Items))
		}
		raw, _ := json.Marshal(result.Items[0].InputSchema)
		if strings.Contains(string(raw), `"required":null`) || strings.Contains(string(raw), `"properties":null`) {
			t.Errorf("%s: null keyword in %s", name, raw)
		}
		if problems := ValidateToolForClient(&mcpsdk.Tool{Name: "x", InputSchema: result.Items[0].InputSchema}); len(problems) != 0 {
			t.Errorf("%s: %v", name, problems)
		}
	}
	if !reflect.DeepEqual(bad.InputSchema, nullRequiredSchema()) {
		t.Fatal("registry definition changed")
	}
}
