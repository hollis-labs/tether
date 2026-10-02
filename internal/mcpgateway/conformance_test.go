package mcpgateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestConformanceInputSchema(t *testing.T) {
	var fetched atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { fetched.Add(1) }))
	defer srv.Close()
	for _, tc := range []struct {
		name   string
		schema any
		code   string
	}{
		{"object", map[string]any{"type": "object"}, ""},
		{"typed", &jsonschema.Schema{Type: "object"}, ""},
		{"null", nil, "input_schema"},
		{"scalar", true, "input_schema"},
		{"array", map[string]any{"type": "array"}, "input_schema"},
		{"missing_type", map[string]any{}, "input_schema"},
		{"malformed_property", map[string]any{"type": "object", "properties": map[string]any{"x": 123}}, "input_schema"},
		{"negative_bound", map[string]any{"type": "object", "minProperties": -1}, "input_schema"},
		{"bad_regex", map[string]any{"type": "object", "patternProperties": map[string]any{"[": map[string]any{"type": "string"}}}, "input_schema"},
		{"local_ref", map[string]any{"type": "object", "$defs": map[string]any{"x": map[string]any{"type": "string"}}, "properties": map[string]any{"x": map[string]any{"$ref": "#/$defs/x"}}}, ""},
		{"unknown_ref", map[string]any{"type": "object", "properties": map[string]any{"x": map[string]any{"$ref": "#/$defs/missing"}}}, "input_schema"},
		{"external_ref", map[string]any{"type": "object", "properties": map[string]any{"x": map[string]any{"$ref": srv.URL + "/secret-schema"}}}, "input_schema_unexamined"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, _ := json.Marshal(tc.schema)
			tool := &mcpsdk.Tool{Name: "alpha_read", Annotations: &mcpsdk.ToolAnnotations{}, InputSchema: tc.schema}
			findings := LintTool("alpha", tool)
			if tc.code == "" && len(findings) != 0 || tc.code != "" && (len(findings) != 1 || findings[0].Code != tc.code) {
				t.Fatalf("findings %+v, want %q", findings, tc.code)
			}
			after, _ := json.Marshal(tc.schema)
			if string(before) != string(after) {
				t.Fatal("schema rewritten")
			}
			raw, _ := json.Marshal(findings)
			if strings.Contains(string(raw), "secret-schema") || strings.Contains(string(raw), srv.URL) {
				t.Fatal("schema contents leaked")
			}
		})
	}
	if fetched.Load() != 0 {
		t.Fatal("lint fetched external schemas")
	}
}

func TestConformanceInstructionsAreOriginScoped(t *testing.T) {
	if len(LintInstructions("alpha", 2048)) != 0 || len(LintInstructions("alpha", 2049)) != 1 {
		t.Fatal("instructions boundary")
	}
	a, b := "alpha", "beta"
	snapshot := Snapshot{Origins: []OriginStatus{{ID: a, Status: "connected"}, {ID: b, Status: "connected"}}, Lint: append(LintInstructions(a, 2049), LintInstructions(b, 2050)...)}
	for _, servers := range [][]string{{a}, {b}, {}} {
		service := Service{Snapshot: func() Snapshot { return snapshot }, Policy: &Policy{Selection: ProfileSelection{Profile: &Profile{Servers: servers}}}}
		got := service.Status("").Lint
		if len(got) != len(servers) || len(got) > 0 && got[0].Origin != servers[0] {
			t.Fatalf("origin findings leak: %+v servers %v", got, servers)
		}
	}
}

func TestConformanceSchemaBoundsAndReferenceCycles(t *testing.T) {
	nested := map[string]any{"type": "object"}
	for i := 0; i < 10000; i++ {
		nested = map[string]any{"type": "object", "properties": map[string]any{"x": nested}}
	}
	cyclic := map[string]any{"type": "object"}
	cyclic["properties"] = cyclic
	wide := map[string]any{}
	for i := 0; i < 5000; i++ {
		wide[strings.Repeat("x", i)] = map[string]any{"type": "string"}
	}
	for _, value := range []any{nested, cyclic, map[string]any{"type": "object", "description": strings.Repeat("x", 256*1024+1)}, map[string]any{"type": "object", "properties": wide}} {
		if code := lintInputSchema(value); code != "input_schema_limit" {
			t.Fatalf("oversize/cyclic Go schema: %s", code)
		}
	}
	// A recursive local JSON reference is valid and must terminate without
	// trying to expand the referenced graph or validate invented tool arguments.
	refs := map[string]any{"type": "object", "properties": map[string]any{"child": map[string]any{"$ref": "#"}}}
	if code := lintInputSchema(refs); code != "" {
		t.Fatalf("valid recursive local reference: %s", code)
	}
}

func TestConformanceUnexaminedCountRespectsProfileAndLaunchFloor(t *testing.T) {
	snapshot := Snapshot{Origins: []OriginStatus{{ID: "alpha", Status: "connected"}, {ID: "beta", Status: "connected"}}}
	for _, entry := range []Entry{{Origin: "alpha", Tool: &mcpsdk.Tool{Name: "alpha_public"}}, {Origin: "alpha", Tool: &mcpsdk.Tool{Name: "alpha_hidden"}}, {Origin: "beta", Tool: &mcpsdk.Tool{Name: "beta_public"}}} {
		snapshot.Entries = append(snapshot.Entries, entry)
		snapshot.Lint = append(snapshot.Lint, NameFinding{Origin: entry.Origin, Name: entry.Tool.Name, Code: "conformance_unexamined"})
	}
	policy := &Policy{Selection: ProfileSelection{Profile: &Profile{Servers: []string{"alpha"}, Tools: ToolRules{Allow: []string{"alpha_public"}}}}}
	service := Service{Snapshot: func() Snapshot { return snapshot }, Policy: policy}
	got := service.Status("")
	if got.UnexaminedTools != 1 || len(got.Lint) != 1 || got.Lint[0].Name != "alpha_public" {
		t.Fatalf("excluded remainder leaked: %+v", got)
	}
	policy.Floors = []ProfileSelection{{Profile: &Profile{Servers: []string{"beta"}}}}
	got = service.Status("")
	if got.UnexaminedTools != 0 || len(got.Lint) != 0 {
		t.Fatalf("launch floor remainder leaked: %+v", got)
	}
}
