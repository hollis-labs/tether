package mcpadapter

import (
	"context"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/chrispian/agent-mux/internal/store"
)

// registerObservationTools wires the four durable observation surface tools
// onto s. These tools expose per-session history that is persisted in SQLite:
//
//   - mux_session_events       — lifecycle events from the events table
//   - mux_session_checkpoints  — checkpoints via the session's logical agent
//   - mux_session_attachments  — client attach/detach history
//   - mux_proxy_events         — proxied tool call events (same as mux_events_tool_calls
//     but with richer filtering and a stable name)
//
// All four tools are read-only and require no auth scope. They are always
// registered (not proxy-mode-only) because the underlying tables are populated
// in both modes.
func (a *Adapter) registerObservationTools(s *server.MCPServer) {
	a.registerSessionEventsTool(s)
	a.registerSessionCheckpointsTool(s)
	a.registerSessionAttachmentsTool(s)
	a.registerProxyEventsTool(s)
}

// ─── mux_session_events ───────────────────────────────────────────────────────

func (a *Adapter) registerSessionEventsTool(s *server.MCPServer) {
	a.addTool(s,
		mcp.NewTool("mux_session_events",
			mcp.WithDescription(
				"List historical lifecycle events for a session. "+
					"Returns events in descending seq order (newest first).",
			),
			mcp.WithString("session_id",
				mcp.Required(),
				mcp.Description("Session UUID"),
			),
			mcp.WithNumber("limit",
				mcp.Description("Max events to return (default 100, max 1000)"),
			),
			mcp.WithNumber("cursor",
				mcp.Description("Pagination cursor: smallest seq from previous page; omit on first page"),
			),
		),
		func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			sessionID := str(req, "session_id")
			if sessionID == "" {
				return toolError("invalid_request", "session_id required"), nil
			}

			limit := intArg(req, "limit", 100)
			if limit <= 0 {
				limit = 100
			}
			if limit > 1000 {
				limit = 1000
			}

			var cursor int64
			if raw, ok := req.GetArguments()["cursor"]; ok {
				switch v := raw.(type) {
				case float64:
					cursor = int64(v)
				case int64:
					cursor = v
				}
			}

			evs, err := a.svc.Store.ListEventsBySession(sessionID, limit, cursor)
			if err != nil {
				return toolError("internal_error", "list events: "+err.Error()), nil //nolint:nilerr // MCP handler encodes err in tool response; Go error is intentionally nil
			}

			// Build DTO slice to expose consistent field names over the wire.
			type eventDTO struct {
				Seq         int64  `json:"seq"`
				At          string `json:"at"`
				Scope       string `json:"scope"`
				SessionID   string `json:"session_id,omitempty"`
				Kind        string `json:"kind"`
				PayloadJSON string `json:"payload_json,omitempty"`
			}
			out := make([]eventDTO, 0, len(evs))
			for _, ev := range evs {
				out = append(out, eventDTO{
					Seq:         ev.Seq,
					At:          ev.At.UTC().Format(time.RFC3339Nano),
					Scope:       ev.Scope,
					SessionID:   ev.SessionID,
					Kind:        ev.Kind,
					PayloadJSON: ev.PayloadJSON,
				})
			}

			result := map[string]any{
				"ok":          true,
				"events":      out,
				"count":       len(out),
				"next_cursor": int64(0),
			}
			// When we filled the page, the caller may need to continue paging.
			if len(evs) == limit && len(evs) > 0 {
				result["next_cursor"] = evs[len(evs)-1].Seq
			}
			return toolJSON(result), nil
		},
	)
}

// ─── mux_session_checkpoints ─────────────────────────────────────────────────

func (a *Adapter) registerSessionCheckpointsTool(s *server.MCPServer) {
	a.addTool(s,
		mcp.NewTool("mux_session_checkpoints",
			mcp.WithDescription(
				"List checkpoints for a session (via its logical agent). "+
					"Returns newest first.",
			),
			mcp.WithString("session_id",
				mcp.Required(),
				mcp.Description("Session UUID"),
			),
		),
		func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			sessionID := str(req, "session_id")
			if sessionID == "" {
				return toolError("invalid_request", "session_id required"), nil
			}

			row, err := a.svc.Store.GetSession(sessionID)
			if err != nil {
				if isNotFound(err) {
					return toolError("not_found", "session not found: "+sessionID), nil
				}
				return toolError("internal_error", err.Error()), nil
			}

			cps, err := a.svc.Store.ListCheckpointsByLogicalAgent(row.LogicalAgentID)
			if err != nil {
				return toolError("internal_error", "list checkpoints: "+err.Error()), nil //nolint:nilerr // MCP handler encodes err in tool response; Go error is intentionally nil
			}

			return toolJSON(map[string]any{
				"ok":          true,
				"checkpoints": cps,
				"count":       len(cps),
			}), nil
		},
	)
}

// ─── mux_session_attachments ─────────────────────────────────────────────────

func (a *Adapter) registerSessionAttachmentsTool(s *server.MCPServer) {
	a.addTool(s,
		mcp.NewTool("mux_session_attachments",
			mcp.WithDescription(
				"List client attach/detach records for a session.",
			),
			mcp.WithString("session_id",
				mcp.Required(),
				mcp.Description("Session UUID"),
			),
		),
		func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			sessionID := str(req, "session_id")
			if sessionID == "" {
				return toolError("invalid_request", "session_id required"), nil
			}

			rows, err := a.svc.Store.ListClientAttachments(sessionID)
			if err != nil {
				return toolError("internal_error", "list attachments: "+err.Error()), nil //nolint:nilerr // MCP handler encodes err in tool response; Go error is intentionally nil
			}

			type attachmentDTO struct {
				ID         string `json:"id"`
				SessionID  string `json:"session_id"`
				ClientKind string `json:"client_kind"`
				AttachedAt string `json:"attached_at"`
				DetachedAt string `json:"detached_at"`
			}
			out := make([]attachmentDTO, 0, len(rows))
			for _, r := range rows {
				detachedAt := ""
				if r.DetachedAt.Valid {
					detachedAt = r.DetachedAt.String
				}
				out = append(out, attachmentDTO{
					ID:         r.ID,
					SessionID:  r.SessionID,
					ClientKind: r.ClientKind,
					AttachedAt: r.AttachedAt,
					DetachedAt: detachedAt,
				})
			}

			return toolJSON(map[string]any{
				"ok":          true,
				"attachments": out,
				"count":       len(out),
			}), nil
		},
	)
}

// ─── mux_proxy_events ────────────────────────────────────────────────────────

func (a *Adapter) registerProxyEventsTool(s *server.MCPServer) {
	a.addTool(s,
		mcp.NewTool("mux_proxy_events",
			mcp.WithDescription(
				"Query durable proxy/tool call events from the SQLite store. "+
					"Supports filtering by session, server, tool, errors-only, and since cursor.",
			),
			mcp.WithString("session_id",
				mcp.Description("Filter by mux session ID (exact match)"),
			),
			mcp.WithString("server",
				mcp.Description("Filter by upstream server ID (exact match, e.g. 'hadron')"),
			),
			mcp.WithString("tool_name",
				mcp.Description("Filter by tool name prefix (e.g. 'hadron_' matches all hadron tools)"),
			),
			mcp.WithBoolean("errors_only",
				mcp.Description("When true, return only events where the tool call failed"),
			),
			mcp.WithNumber("limit",
				mcp.Description("Max events to return (default 100, max 500)"),
			),
			mcp.WithString("since",
				mcp.Description("RFC3339 lower-bound timestamp; excludes events at or before this time"),
			),
		),
		func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			f := store.ProxyEventFilter{
				SessionID: str(req, "session_id"),
				ServerID:  str(req, "server"),
				ToolName:  str(req, "tool_name"),
			}

			limit := intArg(req, "limit", 100)
			if limit <= 0 {
				limit = 100
			}
			if limit > 500 {
				limit = 500
			}
			f.Limit = limit

			if b, ok := req.GetArguments()["errors_only"].(bool); ok {
				f.ErrorsOnly = b
			}

			if since := str(req, "since"); since != "" {
				t, parseErr := time.Parse(time.RFC3339, since)
				if parseErr != nil {
					return toolError("invalid_request", //nolint:nilerr
						"since must be an RFC3339 timestamp: "+parseErr.Error()), nil
				}
				f.Since = t
			}

			evs, err := a.svc.Store.QueryProxyEvents(f)
			if err != nil {
				return toolError("internal_error", "query proxy events: "+err.Error()), nil //nolint:nilerr // MCP handler encodes err in tool response; Go error is intentionally nil
			}

			return toolJSON(map[string]any{
				"ok":     true,
				"events": evs,
				"count":  len(evs),
			}), nil
		},
	)
}
