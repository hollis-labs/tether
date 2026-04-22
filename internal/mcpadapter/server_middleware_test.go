package mcpadapter

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/chrispian/agent-mux/internal/events"
)

// TestServerLevelMiddlewareFires verifies that registering LoggingMiddleware
// via s.Use() causes tool_call_end events to be published to the bus when
// any tool — native or proxied — is invoked through the MCP server.
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

	// Build an MCP server and register LoggingMiddleware via s.Use().
	lm := NewLoggingMiddleware(bus)
	s := mcpserver.NewMCPServer("test", "0.0.1", mcpserver.WithToolCapabilities(true))
	s.Use(func(next mcpserver.ToolHandlerFunc) mcpserver.ToolHandlerFunc {
		return func(hCtx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return lm.Handle(hCtx, req, ToolCallHandler(next))
		}
	})

	// Register a simple native tool.
	s.AddTool(
		mcp.NewTool("native_test_tool", mcp.WithDescription("test native tool")),
		func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText("native-ok"), nil
		},
	)

	// Use the in-process client to call the tool — this exercises the full
	// server dispatch path including s.Use() middleware.
	c, err := mcpclient.NewInProcessClient(s)
	if err != nil {
		t.Fatalf("NewInProcessClient: %v", err)
	}
	defer c.Close()

	if _, err := c.Initialize(ctx, mcp.InitializeRequest{}); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	req := mcp.CallToolRequest{}
	req.Params.Name = "native_test_tool"
	req.Params.Arguments = map[string]any{}

	result, callErr := c.CallTool(ctx, req)
	if callErr != nil {
		t.Fatalf("CallTool: %v", callErr)
	}
	if result.IsError {
		t.Fatalf("tool returned error result")
	}

	// Collect events — expect exactly one tool_call_end.
	var got *events.ToolCallEvent
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
			got = &tce
			break collect
		case <-deadline:
			break collect
		}
	}

	if got == nil {
		t.Fatal("no tool_call_end event received — s.Use() middleware is not firing for native tools")
	}
	if got.ToolName != "native_test_tool" {
		t.Errorf("ToolName = %q, want native_test_tool", got.ToolName)
	}
	if !got.OK {
		t.Errorf("OK = false, want true (error: %s)", got.Error)
	}
	t.Logf("✓ tool_call_end: tool=%s ok=%v dur=%dms", got.ToolName, got.OK, got.DurationMs)
}
