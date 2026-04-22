package mcpadapter

import (
	"context"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// registerToolCallEventsTool registers the mux_events_tool_calls native tool
// on s. The tool queries the ToolCallEventStore and returns results as JSON.
//
// This tool is only registered in proxy mode when an event store is wired.
// See T-03 (CW-20260422-0010) and ADR 0021.
func registerToolCallEventsTool(s *server.MCPServer, store *ToolCallEventStore) {
	s.AddTool(
		mcp.NewTool("mux_events_tool_calls",
			mcp.WithDescription(
				"Query recent proxied tool call events recorded by agent-mux. "+
					"Returns up to `limit` events (default 50, max 500), newest last. "+
					"Available only in --proxy mode.",
			),
			mcp.WithString("server",
				mcp.Description("Filter by upstream server ID (exact match, e.g. 'hadron')"),
			),
			mcp.WithString("tool_name",
				mcp.Description("Filter by tool name prefix (e.g. 'hadron_' matches all hadron tools)"),
			),
			mcp.WithString("session_id",
				mcp.Description("Filter by mux session ID (exact match)"),
			),
		mcp.WithString("limit",
			mcp.Description("Max events to return (default 50, max 500) — pass as a number or numeric string"),
		),
			mcp.WithString("since",
				mcp.Description("RFC3339 lower-bound timestamp; excludes events at or before this time"),
			),
			mcp.WithBoolean("errors_only",
				mcp.Description("When true, return only events where the tool call failed"),
			),
		),
		func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			f := ToolCallEventFilter{}

			if v := str(req, "server"); v != "" {
				f.ServerID = v
			}
			if v := str(req, "tool_name"); v != "" {
				f.ToolName = v
			}
			if v := str(req, "session_id"); v != "" {
				f.SessionID = v
			}

			limit := intArg(req, "limit", 50)
			if limit > 500 {
				limit = 500
			}
			if limit < 1 {
				limit = 50
			}
			f.Limit = limit

			if since := str(req, "since"); since != "" {
				t, err := time.Parse(time.RFC3339, since)
				if err != nil {
					return toolError("invalid_request",
						"since must be an RFC3339 timestamp: "+err.Error()), nil
				}
				f.SinceTimestamp = t
			}

			if b, ok := req.GetArguments()["errors_only"].(bool); ok {
				f.ErrorsOnly = b
			}

			results := store.Query(f)
			return toolJSON(map[string]any{
				"ok":     true,
				"events": results,
				"count":  len(results),
			}), nil
		},
	)
}
