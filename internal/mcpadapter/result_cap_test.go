package mcpadapter

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/mcpgateway"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func bigListResult(n int) *mcpsdk.CallToolResult {
	items := make([]map[string]any, n)
	for i := range items {
		items[i] = map[string]any{"id": i, "note": strings.Repeat("z", 180)}
	}
	raw, _ := json.Marshal(map[string]any{"ok": true, "data": map[string]any{"items": items}})
	return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: string(raw)}}}
}

// resultCapHarness serves search mode over an in-memory connection with the
// real logging middleware followed by the result cap, as the gateway wires them.
func resultCapHarness(t *testing.T, profile *mcpgateway.Profile) (*mcpsdk.ClientSession, *gatewayEventCapture) {
	t.Helper()
	a := newTestAdapter(t)
	s := a.newBareServer()
	registry := NewToolRegistry()
	schema := map[string]any{"type": "object"}
	read := &mcpsdk.ToolAnnotations{ReadOnlyHint: true}
	mustRegister(t, registry, "alpha", nil, []*mcpsdk.Tool{
		{Name: "alpha_list_widgets", Description: "List widgets.", InputSchema: schema, Annotations: read},
		{Name: "alpha_dump_widgets", Description: "Dump widgets.", InputSchema: schema, Annotations: read},
		{Name: "alpha_create_widgets", Description: "Create widgets and return a receipt.", InputSchema: schema},
	})
	gateway := a.gatewayService(registry, nil, mcpgateway.Selection{Mode: mcpgateway.Search}, nil)
	gateway.Dispatch = func(_ context.Context, name string, _, _ map[string]any) (*mcpsdk.CallToolResult, error) {
		return bigListResult(600), nil
	}
	capture := &gatewayEventCapture{}
	logging := NewLoggingMiddleware(capture)
	chain := []ToolCallMiddleware{logging, a.newResultCapMiddleware(registry, profile)}
	s.SDKServer().AddReceivingMiddleware(proxyLoggingMiddleware(chain, registry))
	a.registerCallTool(s, gateway)
	a.registerResultNextTool(s)
	return connectInMemory(t, s), capture
}

func callText(t *testing.T, res *mcpsdk.CallToolResult) []string {
	t.Helper()
	var out []string
	for _, c := range res.Content {
		out = append(out, c.(*mcpsdk.TextContent).Text)
	}
	return out
}

func TestResultCapCutsAReadThroughToolCallAndNextServesTheRest(t *testing.T) {
	c, capture := resultCapHarness(t, nil)
	ctx := context.Background()
	res, err := c.CallTool(ctx, &mcpsdk.CallToolParams{Name: "tether_tool_call", Arguments: map[string]any{"name": "alpha_list_widgets", "arguments": map[string]any{}}})
	if err != nil || res.IsError {
		t.Fatalf("call: %v %v", res, err)
	}
	texts := callText(t, res)
	if len(texts) != 2 {
		t.Fatalf("want the cut result plus a marker block, got %d blocks", len(texts))
	}
	var first struct {
		Data struct {
			Items []struct{ ID int } `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(texts[0]), &first); err != nil {
		t.Fatalf("cut result is not valid JSON: %v", err)
	}
	var mk struct {
		M struct {
			Cursor    string `json:"next_cursor"`
			Path      string `json:"path"`
			Total     int    `json:"total"`
			Truncated bool   `json:"truncated"`
		} `json:"gateway_truncated"`
	}
	if err := json.Unmarshal([]byte(texts[1]), &mk); err != nil || !mk.M.Truncated || mk.M.Cursor == "" || mk.M.Total != 600 || mk.M.Path != "data.items" {
		t.Fatalf("marker=%s err=%v", texts[1], err)
	}
	if len(texts[0])+len(texts[1]) > mcpgateway.DefaultResultCapBytes {
		t.Fatalf("delivered %d bytes, cap %d", len(texts[0])+len(texts[1]), mcpgateway.DefaultResultCapBytes)
	}

	got := len(first.Data.Items)
	cursor := mk.M.Cursor
	for i := 0; cursor != "" && i < 100; i++ {
		page, err := c.CallTool(ctx, &mcpsdk.CallToolParams{Name: mcpgateway.ResultNextTool, Arguments: map[string]any{"cursor": cursor}})
		if err != nil || page.IsError {
			t.Fatalf("next: %v %v", page, err)
		}
		pt := callText(t, page)
		var body struct {
			Data struct {
				Items []struct{ ID int } `json:"items"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(pt[0]), &body); err != nil {
			t.Fatal(err)
		}
		for j, it := range body.Data.Items {
			if it.ID != got+j {
				t.Fatalf("gap or repeat: want id %d got %d", got+j, it.ID)
			}
		}
		got += len(body.Data.Items)
		var nm struct {
			M struct {
				Cursor string `json:"next_cursor"`
			} `json:"gateway_truncated"`
		}
		_ = json.Unmarshal([]byte(pt[len(pt)-1]), &nm)
		cursor = nm.M.Cursor
	}
	if got != 600 {
		t.Fatalf("collected %d of 600 items", got)
	}

	var call *events.ToolCallEvent
	evs := endEvents(t, capture)
	for i := range evs {
		if evs[i].ToolName == "alpha_list_widgets" {
			call = &evs[i]
		}
	}
	if call == nil || !call.ResultCapped || call.ResultOriginalBytes < 100000 || call.ResultBytes > int64(mcpgateway.DefaultResultCapBytes) {
		t.Fatalf("telemetry should show original vs delivered: %+v", call)
	}
}

func TestResultCapNeverCutsAWriteReceiptOrAnError(t *testing.T) {
	c, capture := resultCapHarness(t, nil)
	res, err := c.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: "tether_tool_call", Arguments: map[string]any{"name": "alpha_create_widgets", "arguments": map[string]any{}}})
	if err != nil || res.IsError {
		t.Fatalf("call: %v %v", res, err)
	}
	if texts := callText(t, res); len(texts) != 1 || len(texts[0]) < 100000 {
		t.Fatalf("a write receipt larger than the cap must arrive whole: %d blocks", len(texts))
	}
	for _, ev := range endEvents(t, capture) {
		if ev.ToolName == "alpha_create_widgets" && ev.ResultCapped {
			t.Fatal("write receipt reported as capped")
		}
	}
}

func TestResultCapProfileAndEnvOverrides(t *testing.T) {
	profile := &mcpgateway.Profile{ResultCap: &mcpgateway.ResultCap{DefaultBytes: 8192, Tools: map[string]int{"alpha_dump_widgets": -1}}}
	c, _ := resultCapHarness(t, profile)
	ctx := context.Background()
	res, _ := c.CallTool(ctx, &mcpsdk.CallToolParams{Name: "tether_tool_call", Arguments: map[string]any{"name": "alpha_list_widgets", "arguments": map[string]any{}}})
	if n := len(strings.Join(callText(t, res), "")); n > 8192 {
		t.Fatalf("profile cap 8192 not applied: delivered %d", n)
	}
	res, _ = c.CallTool(ctx, &mcpsdk.CallToolParams{Name: "tether_tool_call", Arguments: map[string]any{"name": "alpha_dump_widgets", "arguments": map[string]any{}}})
	if texts := callText(t, res); len(texts) != 1 {
		t.Fatalf("a tool set to -1 must be delivered whole, got %d blocks", len(texts))
	}

	t.Setenv(mcpgateway.ResultCapEnv, "0")
	off, _ := resultCapHarness(t, nil)
	res, _ = off.CallTool(ctx, &mcpsdk.CallToolParams{Name: "tether_tool_call", Arguments: map[string]any{"name": "alpha_list_widgets", "arguments": map[string]any{}}})
	if texts := callText(t, res); len(texts) != 1 {
		t.Fatalf("%s=0 must turn the cap off, got %d blocks", mcpgateway.ResultCapEnv, len(texts))
	}
}

func TestResultNextRejectsUnknownCursors(t *testing.T) {
	c, _ := resultCapHarness(t, nil)
	for _, cur := range []string{"", "x", "0000.1", fmt.Sprintf("%032d.5", 0)} {
		res, err := c.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: mcpgateway.ResultNextTool, Arguments: map[string]any{"cursor": cur}})
		if err != nil {
			continue // schema or transport rejection is also a refusal
		}
		if !res.IsError {
			t.Fatalf("cursor %q was accepted", cur)
		}
	}
}
