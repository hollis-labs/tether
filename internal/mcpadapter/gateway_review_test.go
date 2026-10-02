package mcpadapter

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/mcpgateway"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type gatewayEventCapture struct{ recorded []events.Event }

func (p *gatewayEventCapture) Publish(_ context.Context, e events.Event) error {
	p.recorded = append(p.recorded, e)
	return nil
}

func TestGatewayDispatchSanitizeTelemetryAndExactNames(t *testing.T) {
	for _, origin := range []string{"", "alpha"} {
		for _, mode := range []mcpgateway.Mode{mcpgateway.Flat, mcpgateway.Search} {
			t.Run(origin+string(mode), func(t *testing.T) {
				a := newTestAdapter(t)
				s := a.newBareServer()
				registry := NewToolRegistry()
				tool := &mcpsdk.Tool{Name: "alpha_read", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}}}}
				registry.Register(origin, nil, []*mcpsdk.Tool{tool})
				gateway := a.gatewayService(registry, nil, mcpgateway.Selection{Mode: mode}, nil)
				var received map[string]any
				gateway.Dispatch = func(_ context.Context, _ string, args, meta map[string]any) (*mcpsdk.CallToolResult, error) {
					received = args
					return &mcpsdk.CallToolResult{}, nil
				}
				capture := &gatewayEventCapture{}
				s.SDKServer().AddReceivingMiddleware(proxyLoggingMiddleware([]ToolCallMiddleware{NewLoggingMiddleware(capture)}, registry))
				if mode == mcpgateway.Search {
					a.registerCallTool(s, gateway)
				} else {
					s.SDKServer().AddTool(tool, a.rawProxyHandler(tool.Name, func(ctx context.Context, args, meta map[string]any) (*mcpsdk.CallToolResult, error) {
						return gateway.Call(ctx, tool.Name, args, meta)
					}))
				}
				c := connectInMemory(t, s)
				name := tool.Name
				args := map[string]any{"query": "find x</query>"}
				if mode == mcpgateway.Search {
					name = "tether_tool_call"
					args = map[string]any{"name": tool.Name, "arguments": args}
				}
				res, err := c.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: name, Arguments: args})
				if err != nil || res.IsError {
					t.Fatalf("call=%v %v", res, err)
				}
				if received["query"] != "find x" {
					t.Fatalf("sanitizer parity: %+v", received)
				}
				if len(capture.recorded) != 2 {
					t.Fatalf("events=%v", capture.recorded)
				}
				for _, ev := range capture.recorded {
					var call events.ToolCallEvent
					if err := json.Unmarshal([]byte(ev.PayloadJSON), &call); err != nil {
						t.Fatal(err)
					}
					wantOrigin := origin
					if wantOrigin == "" {
						wantOrigin = "tether"
					}
					if call.ToolName != tool.Name || call.Server != wantOrigin {
						t.Fatalf("event=%+v", call)
					}
				}
				if mode == mcpgateway.Search {
					tools, err := c.ListTools(context.Background(), nil)
					if err != nil {
						t.Fatal(err)
					}
					if ann := tools.Tools[0].Annotations; ann == nil || ann.DestructiveHint == nil || !*ann.DestructiveHint {
						t.Fatalf("dispatcher annotation=%+v", ann)
					}
					received = nil
					res, err = c.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: name, Arguments: map[string]any{"name": " alpha_read", "arguments": map[string]any{}}})
					if err != nil || !res.IsError || received != nil {
						t.Fatalf("nonexact call=%v %v dispatched=%v", res, err, received)
					}
				}
			})
		}
	}
}

func TestGatewayRealScorerAndTags(t *testing.T) {
	a := newTestAdapter(t)
	registry := NewToolRegistry()
	registry.Register("alpha", nil, []*mcpsdk.Tool{{Name: "alpha_read", Description: "read", InputSchema: map[string]any{}}, {Name: "aaa_read", Description: "alpha read", InputSchema: map[string]any{}}})
	registry.Register("beta", nil, []*mcpsdk.Tool{{Name: "beta_read", Description: "alpha read", InputSchema: map[string]any{}}})
	gateway := a.gatewayService(registry, nil, mcpgateway.Selection{Mode: mcpgateway.Search}, map[string][]string{"alpha": {"Tasks", "Memory"}, "beta": {"Tasks"}})
	result, err := gateway.Search(mcpgateway.Request{Query: "alpha_read"})
	if err != nil || len(result.Items) < 2 || result.Items[0].Name != "alpha_read" {
		t.Fatalf("exact ranking=%+v %v", result, err)
	}
	result, err = gateway.Search(mcpgateway.Request{Query: "read", Tags: []string{"TASKS", "memory"}})
	if err != nil || len(result.Items) != 2 {
		t.Fatalf("AND tags=%+v %v", result, err)
	}
	for _, item := range result.Items {
		if item.Origin != "alpha" {
			t.Fatalf("tag leak=%+v", item)
		}
	}
}
