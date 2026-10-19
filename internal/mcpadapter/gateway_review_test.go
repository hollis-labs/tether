package mcpadapter

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/hollis-labs/libs/harness/core/adapters/runtimeevents/catalog"

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
				mustRegister(t, registry, origin, nil, []*mcpsdk.Tool{tool})
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
				if len(capture.recorded) != 4 {
					t.Fatalf("want one legacy and one canonical pair, got %v", capture.recorded)
				}
				wantOrigin := origin
				if wantOrigin == "" {
					wantOrigin = "tether"
				}
				wantKinds := []string{events.EventTypeToolCallStart, catalog.KindToolCallStarted, events.EventTypeToolCallEnd, catalog.KindToolCallFinished}
				var legacyStart, legacyEnd events.ToolCallEvent
				for i, ev := range capture.recorded {
					if ev.Kind != wantKinds[i] {
						t.Fatalf("phase %d kind=%q want %q", i, ev.Kind, wantKinds[i])
					}
					if strings.Contains(ev.PayloadJSON, "find x") || strings.Contains(ev.PayloadJSON, "</query>") {
						t.Fatal("argument content retained in telemetry")
					}
					if ev.Kind == events.EventTypeToolCallStart || ev.Kind == events.EventTypeToolCallEnd {
						var call events.ToolCallEvent
						if err := json.Unmarshal([]byte(ev.PayloadJSON), &call); err != nil {
							t.Fatal(err)
						}
						if call.ToolName != tool.Name || call.Server != wantOrigin {
							t.Fatalf("legacy event=%+v", call)
						}
						if ev.Kind == events.EventTypeToolCallStart {
							legacyStart = call
						} else {
							legacyEnd = call
						}
						continue
					}
					entry, found := catalog.Lookup(ev.Kind)
					if !found {
						t.Fatalf("missing catalog kind %s", ev.Kind)
					}
					raw, err := catalog.PayloadSchema(entry.PayloadSchema)
					if err != nil {
						t.Fatal(err)
					}
					var schema jsonschema.Schema
					if err = json.Unmarshal(raw, &schema); err != nil {
						t.Fatal(err)
					}
					resolved, err := schema.Resolve(nil)
					if err != nil {
						t.Fatal(err)
					}
					var payload map[string]any
					if err = json.Unmarshal([]byte(ev.PayloadJSON), &payload); err != nil {
						t.Fatal(err)
					}
					if err = resolved.Validate(payload); err != nil {
						t.Fatal(err)
					}
					if payload["tool"] != tool.Name || payload["server"] != wantOrigin {
						t.Fatalf("canonical exact name/origin=%v", payload)
					}
					for key := range payload {
						switch key {
						case "tool", "server", "args_bytes", "args_schema_fp", "ok", "error_class", "duration_ms", "gateway_ms", "forward_ms", "result_bytes":
						default:
							t.Fatalf("canonical content field %s", key)
						}
					}
					if payload["args_bytes"].(float64) <= 0 || payload["args_schema_fp"] == "" {
						t.Fatalf("missing canonical argument metadata: %v", payload)
					}
					if ev.Kind == catalog.KindToolCallStarted {
						if _, ok := payload["ok"]; ok {
							t.Fatal("canonical start carries outcome")
						}
						if len(payload) != 4 {
							t.Fatal("canonical start carries finish metadata")
						}
					} else {
						if payload["ok"] != true || payload["error_class"] != "" || payload["result_bytes"].(float64) <= 0 {
							t.Fatalf("canonical finish outcome=%v", payload)
						}
						if payload["args_bytes"] != float64(legacyEnd.ArgsBytes) || payload["args_schema_fp"] != legacyEnd.ArgsSchemaFP || payload["result_bytes"] != float64(legacyEnd.ResultBytes) || payload["duration_ms"] != float64(legacyEnd.DurationMs) {
							t.Fatal("canonical and legacy metadata disagree")
						}
					}
				}
				if legacyStart.OK || !legacyEnd.OK || legacyEnd.Error != "" || legacyEnd.ErrorClass != "" {
					t.Fatalf("legacy outcomes start=%+v end=%+v", legacyStart, legacyEnd)
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
	mustRegister(t, registry, "alpha", nil, []*mcpsdk.Tool{{Name: "alpha_read", Description: "read", InputSchema: map[string]any{}}, {Name: "aaa_read", Description: "alpha read", InputSchema: map[string]any{}}})
	mustRegister(t, registry, "beta", nil, []*mcpsdk.Tool{{Name: "beta_read", Description: "alpha read", InputSchema: map[string]any{}}})
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
