package mcpadapter

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	gomcp "github.com/hollis-labs/go-mcp/server"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hollis-labs/tether/internal/events"
)

// TestServerLevelMiddlewareFires verifies that registering LoggingMiddleware
// as a receiving middleware (proxyLoggingMiddleware, the replacement for
// mark3labs' s.Use()) causes tool_call_end events to be published to the bus
// when any tool — native or proxied — is invoked through the MCP server.
//
// This is the critical integration test for the TUI Activity feed: if this
// test passes, events will flow to the daemon and appear in the feed.
func TestServerLevelMiddlewareFires(t *testing.T) {
	bus := events.NewBus(events.BusOptions{Persister: &fakeEventPersister{}})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// Subscribe before any tools are called.
	ch, unsub, err := bus.Subscribe(ctx, events.Filter{})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer unsub()

	// Build an MCP server and register LoggingMiddleware as a receiving
	// middleware, exactly as RunWithProxyOpts wires it.
	lm := NewLoggingMiddleware(bus)
	s := gomcp.NewServer("test", "0.0.1")
	s.SDKServer().AddReceivingMiddleware(proxyLoggingMiddleware([]ToolCallMiddleware{lm}))

	// Register a simple native tool.
	s.RegisterTool(gomcp.Tool{
		Name:         "native_test_tool",
		Description:  "test native tool",
		InputSchema:  gomcp.EmptyObjectSchema(),
		Handler:      func(context.Context, map[string]any) (any, error) { return "native-ok", nil },
		ReadOnlyHint: true,
	})

	// Use an in-memory client to call the tool — this exercises the full
	// server dispatch path including the receiving middleware.
	c := connectInMemory(t, s)

	result, callErr := c.CallTool(ctx, &mcpsdk.CallToolParams{Name: "native_test_tool", Arguments: map[string]any{}})
	if callErr != nil {
		t.Fatalf("CallTool: %v", callErr)
	}
	if result.IsError {
		t.Fatalf("tool returned error result")
	}

	// Collect events — expect exactly one tool_call_end.
	var got events.ToolCallEvent
	found := false
	deadline := time.After(2 * time.Second)
collect:
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				break collect
			}
			if ev.Kind != events.EventTypeToolCallEnd {
				continue
			}
			var tce events.ToolCallEvent
			if jsonErr := json.Unmarshal([]byte(ev.PayloadJSON), &tce); jsonErr != nil {
				t.Errorf("unmarshal ToolCallEvent: %v", jsonErr)
				continue
			}
			got = tce
			found = true
			break collect
		case <-deadline:
			break collect
		}
	}

	if !found {
		t.Fatal("no tool_call_end event received — the receiving middleware is not firing for native tools")
	}
	if got.ToolName != "native_test_tool" {
		t.Errorf("ToolName = %q, want native_test_tool", got.ToolName)
	}
	if !got.OK {
		t.Errorf("OK = false, want true (error: %s)", got.Error)
	}
	t.Logf("✓ tool_call_end: tool=%s ok=%v dur=%dms", got.ToolName, got.OK, got.DurationMs)
}
