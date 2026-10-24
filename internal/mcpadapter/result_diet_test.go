package mcpadapter

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// The advertised schema and a real call must agree on the detail levels
// (CW-20261024-0050): names, summary (default) and schema.
func TestSearchToolAdvertisesAndHonoursDetailLevels(t *testing.T) {
	c, _ := searchTelemetryHarness(t)
	ctx := context.Background()

	tools, err := c.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var enum []any
	for _, tool := range tools.Tools {
		if tool.Name != "tether_tool_search" {
			continue
		}
		raw, _ := json.Marshal(tool.InputSchema)
		var schema struct {
			Properties struct {
				Detail struct {
					Enum    []any  `json:"enum"`
					Default string `json:"default"`
				} `json:"detail"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatal(err)
		}
		enum = schema.Properties.Detail.Enum
		if schema.Properties.Detail.Default != "summary" {
			t.Fatalf("default detail = %q", schema.Properties.Detail.Default)
		}
	}
	if len(enum) != 3 || enum[0] != "names" || enum[1] != "summary" || enum[2] != "schema" {
		t.Fatalf("advertised detail levels = %v", enum)
	}

	call := func(args map[string]any) (string, map[string]any) {
		res, err := c.CallTool(ctx, &mcpsdk.CallToolParams{Name: "tether_tool_search", Arguments: args})
		if err != nil || res.IsError {
			t.Fatalf("search %v: %v %v", args, res, err)
		}
		text := textOf(res)
		var out map[string]any
		if err := json.Unmarshal([]byte(text), &out); err != nil {
			t.Fatalf("not JSON: %v\n%s", err, text)
		}
		return text, out
	}
	namesText, names := call(map[string]any{"query": "widgets", "detail": "names"})
	summaryText, summary := call(map[string]any{"query": "widgets"})
	schemaText, _ := call(map[string]any{"query": "widgets", "detail": "schema"})
	if len(namesText) >= len(summaryText) || len(summaryText) >= len(schemaText) {
		t.Fatalf("response sizes must grow with detail: %d %d %d", len(namesText), len(summaryText), len(schemaText))
	}
	if strings.Contains(namesText, "inputSchema") || strings.Contains(namesText, "description") {
		t.Fatalf("names detail leaked description or schema: %s", namesText)
	}
	first := names["items"].([]any)[0].(map[string]any)
	if first["name"] == nil || first["origin"] == nil {
		t.Fatalf("names item lacks name or origin: %v", first)
	}
	if strings.Contains(summaryText, "inputSchema") {
		t.Fatalf("summary carried an input schema: %s", summaryText)
	}
	item := summary["items"].([]any)[0].(map[string]any)
	if item["description"] == nil || item["required_args"] == nil {
		t.Fatalf("summary item lacks a description or required_args: %v", item)
	}
	if !strings.Contains(schemaText, "inputSchema") {
		t.Fatalf("schema detail lacks inputSchema")
	}
	// An unknown level is refused, either as a protocol error (schema enum) or an error result.
	if res, err := c.CallTool(ctx, &mcpsdk.CallToolParams{Name: "tether_tool_search", Arguments: map[string]any{"query": "x", "detail": "everything"}}); err == nil && !res.IsError {
		t.Fatal("an unknown detail level must be refused")
	}
}
