package mcpadapter

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/callcontext"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/mcpgateway"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// searchTelemetryHarness serves search mode over an in-memory MCP connection
// with the real logging middleware and a capturing publisher, so assertions are
// on the events a deployed gateway would record (CW-20261024-0048).
func searchTelemetryHarness(t *testing.T) (*mcpsdk.ClientSession, *gatewayEventCapture) {
	t.Helper()
	a := newTestAdapter(t)
	s := a.newBareServer()
	registry := NewToolRegistry()
	schema := map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}}}
	mustRegister(t, registry, "alpha", nil, []*mcpsdk.Tool{
		{Name: "alpha_read_widgets", Description: "Read widgets from the alpha store", InputSchema: schema},
		{Name: "alpha_write_widgets", Description: "Write widgets to the alpha store", InputSchema: schema},
	})
	gateway := a.gatewayService(registry, nil, mcpgateway.Selection{Mode: mcpgateway.Search}, nil)
	gateway.Dispatch = func(context.Context, string, map[string]any, map[string]any) (*mcpsdk.CallToolResult, error) {
		return &mcpsdk.CallToolResult{}, nil
	}
	capture := &gatewayEventCapture{}
	logging := NewLoggingMiddleware(capture)
	// A label unique to this test keeps the process-wide search linker isolated.
	label := "t-" + strings.ReplaceAll(t.Name(), "/", "-")
	logging.contextDecorator = func(ctx context.Context) context.Context { return callcontext.WithClientLabel(ctx, label) }
	s.SDKServer().AddReceivingMiddleware(proxyLoggingMiddleware([]ToolCallMiddleware{logging}, registry))
	a.registerSearchTool(s, gateway)
	a.registerCallTool(s, gateway)
	return connectInMemory(t, s), capture
}

func endEvents(t *testing.T, capture *gatewayEventCapture) []events.ToolCallEvent {
	t.Helper()
	var out []events.ToolCallEvent
	for _, ev := range capture.recorded {
		if ev.Kind != events.EventTypeToolCallEnd {
			continue
		}
		var call events.ToolCallEvent
		if err := json.Unmarshal([]byte(ev.PayloadJSON), &call); err != nil {
			t.Fatal(err)
		}
		out = append(out, call)
	}
	return out
}

func TestSearchTelemetry_RecordsSearchAndLinksTheNextCall(t *testing.T) {
	c, capture := searchTelemetryHarness(t)
	ctx := context.Background()

	if res, err := c.CallTool(ctx, &mcpsdk.CallToolParams{Name: "tether_tool_search", Arguments: map[string]any{"query": "Read the WIDGETS, widgets!"}}); err != nil || res.IsError {
		t.Fatalf("search: %v %v", res, err)
	}
	if res, err := c.CallTool(ctx, &mcpsdk.CallToolParams{Name: "tether_tool_call", Arguments: map[string]any{"name": "alpha_read_widgets", "arguments": map[string]any{"query": "secret-argument-value"}}}); err != nil || res.IsError {
		t.Fatalf("call: %v %v", res, err)
	}

	var search, call *events.ToolCallEvent
	evs := endEvents(t, capture)
	for i := range evs {
		switch evs[i].ToolName {
		case "tether_tool_search":
			search = &evs[i]
		case "alpha_read_widgets":
			call = &evs[i]
		}
	}
	if search == nil || call == nil {
		t.Fatalf("missing events: search=%v call=%v all=%+v", search, call, evs)
	}
	if search.SearchID == "" || search.SearchResults < 1 || search.SearchZero {
		t.Fatalf("search not recorded: %+v", search.ToolCallDetails)
	}
	if search.SearchTerms != "read the widgets" {
		t.Fatalf("terms = %v (want normalized, deduped, ordered)", search.SearchTerms)
	}
	if search.SearchDetail != "summary" {
		t.Fatalf("detail = %q", search.SearchDetail)
	}
	if call.AfterSearchID != search.SearchID || call.AfterSearchRank < 1 {
		t.Fatalf("call not linked to its search: id=%q rank=%d want id=%q", call.AfterSearchID, call.AfterSearchRank, search.SearchID)
	}
	if call.ClientLabel == "" {
		t.Fatal("client label not recorded on the call")
	}
	for _, ev := range capture.recorded {
		if strings.Contains(ev.PayloadJSON, "secret-argument-value") {
			t.Fatal("argument value retained in telemetry")
		}
	}
}

func TestSearchTelemetry_ZeroResultAndUnlinkedCall(t *testing.T) {
	c, capture := searchTelemetryHarness(t)
	ctx := context.Background()
	if _, err := c.CallTool(ctx, &mcpsdk.CallToolParams{Name: "tether_tool_search", Arguments: map[string]any{"query": "qqqzzz"}}); err != nil {
		t.Fatal(err)
	}
	// A different tool than the search found, immediately after: linked to the
	// latest search with rank 0 (not among its results), not silently unlinked.
	if _, err := c.CallTool(ctx, &mcpsdk.CallToolParams{Name: "tether_tool_call", Arguments: map[string]any{"name": "alpha_write_widgets", "arguments": map[string]any{}}}); err != nil {
		t.Fatal(err)
	}
	var search, call events.ToolCallEvent
	for _, ev := range endEvents(t, capture) {
		switch ev.ToolName {
		case "tether_tool_search":
			search = ev
		case "alpha_write_widgets":
			call = ev
		}
	}
	if search.SearchID == "" || search.SearchResults != 0 || !search.SearchZero {
		t.Fatalf("zero-result not recorded: %+v", search.ToolCallDetails)
	}
	if call.AfterSearchID != search.SearchID || call.AfterSearchRank != 0 {
		t.Fatalf("want latest-search link with rank 0, got id=%q rank=%d", call.AfterSearchID, call.AfterSearchRank)
	}
}
