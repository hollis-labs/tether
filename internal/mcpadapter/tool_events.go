package mcpadapter

import (
	"context"
	"time"

	gomcp "github.com/hollis-labs/go-mcp/server"

	"github.com/hollis-labs/tether/internal/store"
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
//
// Sanitize protection applies via the global receiving middleware installed
// in Adapter.newBareServer, so coverage stays uniform across every MCP tool
// the adapter exposes regardless of registration path.
func (a *Adapter) registerToolCallEventsTool(s *gomcp.Server, proxyStore ProxyEventQuerier) {
	a.addTool(s, gomcp.Tool{
		Name: "mux_events_tool_calls",
		Description: "Query recent proxied tool call events recorded by agent-mux. " +
			"Returns up to `limit` events (default 50, max 500), newest last. " +
			"Available only in --proxy mode.",
		InputSchema: gomcp.ObjectSchema(map[string]any{
			"server":      strProp("Filter by upstream server ID (exact match, e.g. 'hadron')"),
			"tool_name":   strProp("Filter by tool name prefix (e.g. 'hadron_' matches all hadron tools)"),
			"session_id":  strProp("Filter by mux session ID (exact match)"),
			"limit":       strProp("Max events to return (default 50, max 500) — pass as a number or numeric string"),
			"since":       strProp("RFC3339 lower-bound timestamp; excludes events at or before this time"),
			"errors_only": boolProp("When true, return only events where the tool call failed"),
		}),
		Handler: func(_ context.Context, args map[string]any) (any, error) {
			f := store.ProxyEventFilter{}

			if v := str(args, "server"); v != "" {
				f.ServerID = v
			}
			if v := str(args, "tool_name"); v != "" {
				f.ToolName = v
			}
			if v := str(args, "session_id"); v != "" {
				f.SessionID = v
			}

			limit := intArg(args, "limit", 50)
			if limit > 500 {
				limit = 500
			}
			if limit < 1 {
				limit = 50
			}
			f.Limit = limit

			if since := str(args, "since"); since != "" {
				t, parsedErr := time.Parse(time.RFC3339, since)
				if parsedErr != nil {
					return nil, toolError("invalid_request",
						"since must be an RFC3339 timestamp: "+parsedErr.Error())
				}
				f.Since = t
			}

			if b, ok := args["errors_only"].(bool); ok {
				f.ErrorsOnly = b
			}

			results, err := proxyStore.QueryProxyEvents(f)
			if err != nil {
				return nil, toolError("internal_error", "query proxy events: "+err.Error())
			}
			return toolJSON(map[string]any{
				"ok":     true,
				"events": results,
				"count":  len(results),
			}), nil
		},
	}, Reads("proxy_events query"))
}
