package mcpadapter

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/mcpgateway"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestGatewayAuthoredMetadataSurvivesFlatSearchAndStatus(t *testing.T) {
	for _, annotated := range []bool{false, true} {
		for _, mode := range []mcpgateway.Mode{mcpgateway.Flat, mcpgateway.Search} {
			t.Run(string(mode)+map[bool]string{true: "/annotated", false: "/unassessed"}[annotated], func(t *testing.T) {
				a := newTestAdapter(t)
				s := a.newBareServer()
				tool := &mcpsdk.Tool{Name: "alpha_lookup", Title: "Authored lookup", Description: "run start post a lookup", InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object", "properties": map[string]any{"authored": map[string]any{"type": "string"}}}, Meta: mcpsdk.Meta{"authored": "untouched"}}
				if annotated {
					no := false
					tool.Annotations = &mcpsdk.ToolAnnotations{Title: "Annotation title", ReadOnlyHint: true, DestructiveHint: &no, OpenWorldHint: &no, IdempotentHint: true}
				}
				registry := NewToolRegistry()
				mustRegister(t, registry, "alpha", nil, []*mcpsdk.Tool{tool})
				gateway := a.gatewayService(registry, nil, mcpgateway.Selection{Mode: mode}, nil)
				s.SDKServer().AddReceivingMiddleware(gatewaySurfaceMiddleware(gateway))
				if mode == mcpgateway.Flat {
					live := liveProxyCatalog{adapter: a, server: s, registry: registry}
					live.addProxyTools(tool)
				} else {
					a.registerSearchTool(s, gateway)
					a.registerListTool(s, gateway)
				}
				a.registerGatewayStatus(s, gateway)
				cs := connectInMemory(t, s)
				ctx := context.Background()
				if mode == mcpgateway.Flat {
					listed, err := cs.ListTools(ctx, nil)
					if err != nil {
						t.Fatal(err)
					}
					found := false
					for _, got := range listed.Tools {
						if got.Name == tool.Name {
							wantJSON, _ := json.Marshal(tool)
							gotJSON, _ := json.Marshal(got)
							if string(gotJSON) != string(wantJSON) {
								t.Fatalf("metadata rewritten: got=%s want=%s", gotJSON, wantJSON)
							}
							found = true
						}
					}
					if !found {
						t.Fatal("authored tool missing")
					}
				} else {
					for name, args := range map[string]map[string]any{"tether_tool_search": {"query": "lookup", "detail": "schema"}, "tether_tool_list": {"names": []string{tool.Name}}} {
						result, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: name, Arguments: args})
						if err != nil {
							t.Fatal(err)
						}
						raw, _ := json.Marshal(parseToolJSON(t, result))
						var listed mcpgateway.Result
						if err := json.Unmarshal(raw, &listed); err != nil || len(listed.Items) != 1 {
							t.Fatalf("%s: %s %v", name, raw, err)
						}
						got := listed.Items[0]
						wantJSON, _ := json.Marshal(mcpgateway.Item{Name: tool.Name, Origin: "alpha", Title: tool.Title, Description: tool.Description, Annotations: tool.Annotations, InputSchema: tool.InputSchema, OutputSchema: tool.OutputSchema, Meta: tool.Meta})
						got.Score = nil
						gotJSON, _ := json.Marshal(got)
						if string(gotJSON) != string(wantJSON) {
							t.Fatalf("%s metadata rewritten: got=%s want=%s", name, gotJSON, wantJSON)
						}
					}
				}
				status, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{Name: "tether_gateway_status", Arguments: map[string]any{"name": tool.Name}})
				if err != nil {
					t.Fatal(err)
				}
				raw, _ := json.Marshal(parseToolJSON(t, status))
				var got mcpgateway.Status
				if err := json.Unmarshal(raw, &got); err != nil || got.Tool == nil || got.Tool.Origin != "alpha" || (got.Tool.Annotations == nil) != !annotated || strings.Contains(string(raw), `"safety"`) {
					t.Fatalf("guessed or missing status metadata: %s %v", raw, err)
				}
			})
		}
	}
}

func TestUnavailableUpstreamCountsSeparateAcceptedCacheFromObservation(t *testing.T) {
	a := newTestAdapter(t)
	registry := NewToolRegistry()
	pool := NewClientPool([]config.MCPServerEntry{{ID: "alpha", Transport: "http"}, {ID: "empty", Transport: "http"}, {ID: "failed", Transport: "http"}}, registry)
	pool.SetConnectFunc(func(_ context.Context, entry config.MCPServerEntry) (upstreamClient, error) {
		if entry.ID == "failed" {
			return nil, errors.New("initialize failed")
		}
		return &mockClient{listToolsFunc: func(context.Context, *mcpsdk.ListToolsParams) (*mcpsdk.ListToolsResult, error) {
			if entry.ID == "empty" {
				return &mcpsdk.ListToolsResult{Tools: []*mcpsdk.Tool{}}, nil
			}
			return &mcpsdk.ListToolsResult{Tools: []*mcpsdk.Tool{makeTool("alpha_read")}}, nil
		}}, nil
	})
	if err := pool.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer pool.Shutdown()
	a.upstreams = pool
	gateway := a.gatewayService(registry, nil, mcpgateway.Selection{Mode: mcpgateway.Flat}, nil)
	for _, status := range pool.StatusSummary() {
		if status.ID == "empty" && (status.ToolCount != 0 || !status.InventoryExamined || status.Status != "connected") {
			t.Fatalf("observed empty inventory reported unexamined: %+v", status)
		}
		if status.ID == "failed" && (status.ToolCount != 0 || status.InventoryExamined || status.Error == "") {
			t.Fatalf("unexamined origin presented as empty observation: %+v", status)
		}
	}
	rt, _ := registry.Lookup("alpha_read")
	pool.fail("alpha", rt.Client, errors.New("connection lost"))
	status := pool.StatusSummary()[0]
	if status.ToolCount != 0 || status.CatalogedTools != 1 || !status.InventoryExamined {
		t.Fatalf("stale available count or lost cache: %+v", status)
	}
	view := gateway.Status("alpha_read")
	if view.CatalogedTools != 1 || view.AvailableTools != 0 || view.Complete || view.Reason != "origin unavailable" || !view.Origins[0].InventoryExamined || view.Origins[0].AvailableTools != 0 {
		t.Fatalf("gateway counts=%+v", view)
	}
}
