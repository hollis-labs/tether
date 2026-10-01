package mcpgateway

import (
	"context"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"reflect"
	"strings"
	"testing"
)

func inventoryService() (*Service, *Snapshot) {
	snapshot := &Snapshot{Origins: []OriginStatus{{ID: "a", Status: "connected", ToolCount: 2}, {ID: "b", Status: "failed", Error: "offline"}}, Entries: []Entry{
		{Tool: &mcpsdk.Tool{Name: "a_two", Title: "Two", Description: "task two", Annotations: &mcpsdk.ToolAnnotations{ReadOnlyHint: true}, InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"}, Meta: mcpsdk.Meta{"owner": "a"}}, Origin: "a", Tags: []string{"tasks"}},
		{Tool: &mcpsdk.Tool{Name: "a_one", Description: "task one", InputSchema: map[string]any{"type": "object"}}, Origin: "a", Tags: []string{"tasks"}},
	}}
	svc := &Service{Selection: Selection{Search, "argument"}, Snapshot: func() Snapshot { return *snapshot }, Score: func(query string, entry Entry) int {
		if strings.Contains(entry.Tool.Description, query) {
			return 1
		}
		return 0
	}}
	return svc, snapshot
}
func TestEnumerationHydrationAndCursorBinding(t *testing.T) {
	svc, snapshot := inventoryService()
	first, err := svc.List(Request{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if first.Items[0].Name != "a_one" || !first.Truncated || first.Complete || !reflect.DeepEqual(first.UnavailableServers, []string{"b"}) {
		t.Fatalf("page: %+v", first)
	}
	last, err := svc.List(Request{Limit: 1, Cursor: first.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	if last.Items[0].Name != "a_two" || last.Truncated || last.Items[0].InputSchema == nil {
		t.Fatalf("last: %+v", last)
	}
	for _, req := range []Request{{Limit: 2, Cursor: first.NextCursor}, {Limit: 1, Servers: []string{"a"}, Cursor: first.NextCursor}} {
		if _, err := svc.List(req); err == nil {
			t.Fatal("changed request cursor accepted")
		}
	}
	snapshot.Entries[0].Tool.Description = "updated schema description"
	if _, err := svc.List(Request{Limit: 1, Cursor: first.NextCursor}); err == nil {
		t.Fatal("stale inventory cursor accepted")
	}
	hydrated, err := svc.List(Request{Names: []string{"a_two", "excluded_tool"}})
	if err != nil {
		t.Fatal(err)
	}
	if hydrated.Items[0].Annotations != snapshot.Entries[0].Tool.Annotations || hydrated.Items[0].Meta["owner"] != "a" || hydrated.Items[1].Error == "" {
		t.Fatalf("hydration: %+v", hydrated)
	}
	if _, err := svc.List(Request{Names: []string{"a_one"}, Servers: []string{"a"}}); err == nil {
		t.Fatal("hydration accepted enumeration fields")
	}
	if _, err := svc.List(Request{Servers: []string{"excluded"}}); err == nil {
		t.Fatal("unknown server accepted")
	}
}
func TestSearchDetailAndUnavailableCalls(t *testing.T) {
	svc, snapshot := inventoryService()
	summary, err := svc.Search(Request{Query: "task", Tags: []string{"tasks"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Items) != 2 || summary.Items[0].InputSchema != nil || summary.Items[0].Score == nil {
		t.Fatalf("summary: %+v", summary)
	}
	schemas, err := svc.Search(Request{Query: "task", Detail: "schema"})
	if err != nil || schemas.Items[1].InputSchema == nil {
		t.Fatalf("schemas %+v %v", schemas, err)
	}
	for _, req := range []Request{{Query: ""}, {Query: "task", Detail: "unknown"}, {Query: "task", Limit: 51}} {
		if _, err := svc.Search(req); err == nil {
			t.Fatalf("invalid request accepted: %+v", req)
		}
	}
	dispatches := 0
	expected := &mcpsdk.CallToolResult{IsError: true, Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "upstream error"}}, StructuredContent: map[string]any{"code": "upstream"}}
	svc.Dispatch = func(context.Context, string, map[string]any, map[string]any) (*mcpsdk.CallToolResult, error) {
		dispatches++
		return expected, nil
	}
	result, err := svc.Call(context.Background(), "a_one", map[string]any{}, nil)
	if err != nil || result != expected {
		t.Fatalf("result not preserved: %v %v", result, err)
	}
	snapshot.Origins[0].Status = "reconnecting"
	if _, err := svc.Call(context.Background(), "a_one", nil, nil); err == nil {
		t.Fatal("unavailable call dispatched")
	}
	if _, err := svc.Call(context.Background(), "excluded_tool", nil, nil); err == nil {
		t.Fatal("excluded call dispatched")
	}
	if dispatches != 1 {
		t.Fatalf("dispatched %d times", dispatches)
	}
	unavailable, err := svc.List(Request{Names: []string{"a_one"}})
	if err != nil || unavailable.Items[0].InputSchema != nil || unavailable.Items[0].Error == "" {
		t.Fatalf("unavailable schema leaked: %+v %v", unavailable, err)
	}
}
