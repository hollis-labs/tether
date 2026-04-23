package mcpadapter

import (
	"context"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/chrispian/agent-mux/internal/store"
)

// toolCallEventStoreQuerier wraps the in-memory ToolCallEventStore to satisfy
// ProxyEventQuerier. Used as a fallback when ProxyStore is not set in
// ProxyOptions so existing callers that only wire EventStore continue working.
type toolCallEventStoreQuerier struct {
	store *ToolCallEventStore
}

// QueryProxyEvents maps store.ProxyEventFilter to ToolCallEventFilter and
// queries the in-memory ring buffer. This is a best-effort bridge — fields
// that have no equivalent in ToolCallEventFilter are ignored.
func (q *toolCallEventStoreQuerier) QueryProxyEvents(f store.ProxyEventFilter) ([]store.ProxyEvent, error) {
	filter := ToolCallEventFilter{
		ServerID:       f.ServerID,
		ToolName:       f.ToolName,
		SessionID:      f.SessionID,
		Limit:          f.Limit,
		SinceTimestamp: f.Since,
		ErrorsOnly:     f.ErrorsOnly,
	}
	events := q.store.Query(filter)
	out := make([]store.ProxyEvent, 0, len(events))
	for _, ev := range events {
		out = append(out, store.ProxyEvent{
			SessionID:    ev.SessionID,
			Server:       ev.Server,
			ToolName:     ev.ToolName,
			ArgsSchemaFP: ev.ArgsSchemaFP,
			DurationMs:   ev.DurationMs,
			OK:           ev.OK,
			Error:        ev.Error,
			Timestamp:    ev.Timestamp,
		})
	}
	return out, nil
}

// ProxyEventQuerier is the narrow store contract that registerToolCallEventsTool
// depends on. *store.Store satisfies it. Defined here to keep tool_events.go
// self-contained and avoid importing the full store package at the call site.
type ProxyEventQuerier interface {
	QueryProxyEvents(f store.ProxyEventFilter) ([]store.ProxyEvent, error)
}

// registerToolCallEventsTool registers the mux_events_tool_calls native tool
// on s. The tool queries the durable proxy_events SQLite table and returns
// results as JSON.
//
// This tool is only registered in proxy mode when a store is wired.
// The in-memory ToolCallEventStore is retained for the live TUI feed (ADR 0021)
// but mux_events_tool_calls now reads from the durable proxy_events table
// (ADR 0024 §4).
func registerToolCallEventsTool(s *server.MCPServer, proxyStore ProxyEventQuerier) {
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
			f := store.ProxyEventFilter{}

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
				t, parsedErr := time.Parse(time.RFC3339, since)
				if parsedErr != nil {
					// toolError returns a non-nil *CallToolResult with IsError:true;
					// the nil is the Go error return — correct MCP pattern.
					return toolError("invalid_request", //nolint:nilerr
						"since must be an RFC3339 timestamp: "+parsedErr.Error()), nil
				}
				f.Since = t
			}

			if b, ok := req.GetArguments()["errors_only"].(bool); ok {
				f.ErrorsOnly = b
			}

			results, err := proxyStore.QueryProxyEvents(f)
			if err != nil {
				return toolError("internal_error", "query proxy events: "+err.Error()), nil
			}
			return toolJSON(map[string]any{
				"ok":     true,
				"events": results,
				"count":  len(results),
			}), nil
		},
	)
}
