package mcpadapter

import (
	"context"
	"errors"
	"strings"
	"time"

	gomcp "github.com/hollis-labs/go-mcp/server"

	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/store"
)

// registerObservationTools wires the observation surface tools
// onto s. These tools expose per-session history that is persisted in SQLite:
//
//   - mux_session_events       — lifecycle events from the events table
//   - mux_session_checkpoints  — checkpoints via the session's logical agent
//   - mux_session_attachments  — client attach/detach history
//   - mux_proxy_events         — proxied tool call events (same as mux_events_tool_calls
//     but with richer filtering and a stable name)
//   - mux_events_history       — broader durable event history across daemon/session/broker scopes
//
// All tools are read-only and require no auth scope. They are always
// registered (not proxy-mode-only) because the underlying tables are populated
// in both modes.
func (a *Adapter) registerObservationTools(s *gomcp.Server) {
	a.registerSessionEventsTool(s)
	a.registerSessionCheckpointsTool(s)
	a.registerSessionAttachmentsTool(s)
	a.registerProxyEventsTool(s)
	a.registerEventsHistoryTool(s)
	a.registerEventsWaitTool(s)
}

// ─── mux_session_events ───────────────────────────────────────────────────────

func (a *Adapter) registerSessionEventsTool(s *gomcp.Server) {
	a.addTool(s, gomcp.Tool{
		Name: "mux_session_events",
		Description: "List historical lifecycle events for a session. " +
			"Returns events in descending seq order (newest first).",
		InputSchema: gomcp.ObjectSchema(map[string]any{
			"session_id": strProp("Session UUID"),
			"limit":      numProp("Max events to return (default 100, max 1000)"),
			"cursor":     numProp("Pagination cursor: smallest seq from previous page; omit on first page"),
		}, "session_id"),
		Handler: func(_ context.Context, args map[string]any) (any, error) {
			sessionID := str(args, "session_id")
			if sessionID == "" {
				return nil, toolError("invalid_request", "session_id required")
			}

			limit := intArg(args, "limit", 100)
			if limit <= 0 {
				limit = 100
			}
			if limit > 1000 {
				limit = 1000
			}

			var cursor int64
			if raw, ok := args["cursor"]; ok {
				switch v := raw.(type) {
				case float64:
					cursor = int64(v)
				case int64:
					cursor = v
				}
			}

			evs, err := a.svc.Store.ListEventsBySession(sessionID, limit, cursor)
			if err != nil {
				return nil, toolError("internal_error", "list events: "+err.Error())
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
	}, Reads("GET /sessions/{id}/events"))
}

// ─── mux_session_checkpoints ─────────────────────────────────────────────────

func (a *Adapter) registerSessionCheckpointsTool(s *gomcp.Server) {
	a.addTool(s, gomcp.Tool{
		Name: "mux_session_checkpoints",
		Description: "List checkpoints for a session (via its logical agent). " +
			"Returns newest first.",
		InputSchema: gomcp.ObjectSchema(map[string]any{
			"session_id": strProp("Session UUID"),
		}, "session_id"),
		Handler: func(_ context.Context, args map[string]any) (any, error) {
			sessionID := str(args, "session_id")
			if sessionID == "" {
				return nil, toolError("invalid_request", "session_id required")
			}

			row, err := a.svc.Store.GetSession(sessionID)
			if err != nil {
				if isNotFound(err) {
					return nil, toolError("not_found", "session not found: "+sessionID)
				}
				return nil, toolError("internal_error", err.Error())
			}

			cps, err := a.svc.Store.ListCheckpointsByLogicalAgent(row.LogicalAgentID)
			if err != nil {
				return nil, toolError("internal_error", "list checkpoints: "+err.Error())
			}

			return toolJSON(map[string]any{
				"ok":          true,
				"checkpoints": cps,
				"count":       len(cps),
			}), nil
		},
	}, Reads("GET /sessions/{id}/checkpoints"))
}

// ─── mux_session_attachments ─────────────────────────────────────────────────

func (a *Adapter) registerSessionAttachmentsTool(s *gomcp.Server) {
	a.addTool(s, gomcp.Tool{
		Name:        "mux_session_attachments",
		Description: "List client attach/detach records for a session.",
		InputSchema: gomcp.ObjectSchema(map[string]any{
			"session_id": strProp("Session UUID"),
		}, "session_id"),
		Handler: func(_ context.Context, args map[string]any) (any, error) {
			sessionID := str(args, "session_id")
			if sessionID == "" {
				return nil, toolError("invalid_request", "session_id required")
			}

			rows, err := a.svc.Store.ListClientAttachments(sessionID)
			if err != nil {
				return nil, toolError("internal_error", "list attachments: "+err.Error())
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
	}, Reads("GET /sessions/{id}/attachments"))
}

// ─── mux_proxy_events ────────────────────────────────────────────────────────

func (a *Adapter) registerProxyEventsTool(s *gomcp.Server) {
	a.addTool(s, gomcp.Tool{
		Name: "mux_proxy_events",
		Description: "Query durable proxy/tool call events from the SQLite store. " +
			"Supports filtering by session, server, tool, errors-only, and since cursor.",
		InputSchema: gomcp.ObjectSchema(map[string]any{
			"session_id":  strProp("Filter by mux session ID (exact match)"),
			"server":      strProp("Filter by upstream server ID (exact match, e.g. 'hadron')"),
			"tool_name":   strProp("Filter by tool name prefix (e.g. 'hadron_' matches all hadron tools)"),
			"errors_only": boolProp("When true, return only events where the tool call failed"),
			"limit":       numProp("Max events to return (default 100, max 500)"),
			"since":       strProp("RFC3339 lower-bound timestamp; excludes events at or before this time"),
		}),
		Handler: func(_ context.Context, args map[string]any) (any, error) {
			f := store.ProxyEventFilter{
				SessionID: str(args, "session_id"),
				ServerID:  str(args, "server"),
				ToolName:  str(args, "tool_name"),
			}

			limit := intArg(args, "limit", 100)
			if limit <= 0 {
				limit = 100
			}
			if limit > 500 {
				limit = 500
			}
			f.Limit = limit

			if b, ok := args["errors_only"].(bool); ok {
				f.ErrorsOnly = b
			}

			if since := str(args, "since"); since != "" {
				t, parseErr := time.Parse(time.RFC3339, since)
				if parseErr != nil {
					return nil, toolError("invalid_request",
						"since must be an RFC3339 timestamp: "+parseErr.Error())
				}
				f.Since = t
			}

			evs, err := a.svc.Store.QueryProxyEvents(f)
			if err != nil {
				return nil, toolError("internal_error", "query proxy events: "+err.Error())
			}

			return toolJSON(map[string]any{
				"ok":     true,
				"events": evs,
				"count":  len(evs),
			}), nil
		},
	}, Reads("GET /proxy/events"))
}

// ─── mux_events_history ──────────────────────────────────────────────────────

func (a *Adapter) registerEventsHistoryTool(s *gomcp.Server) {
	a.addTool(s, gomcp.Tool{
		Name: "mux_events_history",
		Description: "Query durable daemon/session/broker event history from the shared events table. " +
			"Returns newest first.",
		InputSchema: gomcp.ObjectSchema(map[string]any{
			"scope":      strProp("Optional scope allow-list as comma-separated daemon, session, broker."),
			"kind":       strProp("Optional comma-separated event kind allow-list."),
			"session_id": strProp("Optional exact session id filter."),
			"since_seq":  numProp("Only return events with seq greater than this value."),
			"cursor":     numProp("Pagination cursor; return events with seq less than this value."),
			"limit":      numProp("Max events to return (default 100, max 1000)."),
		}),
		Handler: a.handleEventsHistory,
	}, Reads("GET /events"))
}

func (a *Adapter) handleEventsHistory(ctx context.Context, args map[string]any) (any, error) {
	scopes, err := decodeEventScopesAllowEmpty(args)
	if err != nil {
		return nil, err
	}
	type eventDTO struct {
		Seq         int64  `json:"seq"`
		At          string `json:"at"`
		Scope       string `json:"scope"`
		SessionID   string `json:"session_id,omitempty"`
		Kind        string `json:"kind"`
		PayloadJSON string `json:"payload_json,omitempty"`
	}
	kinds := splitCSVArg(args, "kind")
	sessionID := str(args, "session_id")
	sinceSeq := int64(intArg(args, "since_seq", 0))
	cursor := int64(intArg(args, "cursor", 0))
	limit := intArg(args, "limit", 100)

	var out []eventDTO
	var nextCursor int64
	if a.client != nil {
		rows, err := a.client.EventsHistory(ctx, client.EventsHistoryQuery{
			Scopes:    stringScopes(scopes),
			Kinds:     kinds,
			SessionID: sessionID,
			SinceSeq:  sinceSeq,
			Cursor:    cursor,
			Limit:     limit,
		})
		if err != nil {
			return nil, classifyClientErr(err, "")
		}
		nextCursor = rows.NextCursor
		out = make([]eventDTO, 0, len(rows.Events))
		for _, ev := range rows.Events {
			out = append(out, eventDTO{
				Seq:         ev.Seq,
				At:          ev.At,
				Scope:       ev.Scope,
				SessionID:   ev.SessionID,
				Kind:        ev.Kind,
				PayloadJSON: ev.PayloadJSON,
			})
		}
	} else {
		rows, err := a.svc.Store.QueryEvents(store.EventFilter{
			Scopes:    scopes,
			Kinds:     kinds,
			SessionID: sessionID,
			SinceSeq:  sinceSeq,
			Cursor:    cursor,
			Limit:     limit,
		})
		if err != nil {
			return nil, toolError("internal_error", "query events: "+err.Error())
		}
		out = make([]eventDTO, 0, len(rows))
		for _, ev := range rows {
			out = append(out, eventDTO{
				Seq:         ev.Seq,
				At:          ev.At.UTC().Format(time.RFC3339Nano),
				Scope:       ev.Scope,
				SessionID:   ev.SessionID,
				Kind:        ev.Kind,
				PayloadJSON: ev.PayloadJSON,
			})
		}
		if len(rows) == limit && len(rows) > 0 {
			nextCursor = rows[len(rows)-1].Seq
		}
	}
	return toolJSON(map[string]any{
		"ok":          true,
		"events":      out,
		"count":       len(out),
		"next_cursor": nextCursor,
	}), nil
}

// ─── mux_events_wait ─────────────────────────────────────────────────────────

func (a *Adapter) registerEventsWaitTool(s *gomcp.Server) {
	a.addTool(s, gomcp.Tool{
		Name: "mux_events_wait",
		Description: "Wait briefly for live daemon or session events from the muxd event stream. " +
			"Useful for bounded polling-style MCP flows without maintaining a long-lived SSE connection.",
		InputSchema: gomcp.ObjectSchema(map[string]any{
			"scope":      strProp("Event scope filter. Repeatable via comma-separated values: daemon, session, broker. Defaults to daemon."),
			"kind":       strProp("Optional event kind allow-list. Repeatable via comma-separated values."),
			"session_id": strProp("Optional exact session id filter."),
			"since_seq":  numProp("Only return events with seq greater than this value."),
			"wait_ms":    numProp("Maximum time to wait for events in milliseconds (default 5000)."),
			"max_events": numProp("Maximum matching events to return before stopping (default 1, max 100)."),
		}),
		Handler: a.handleEventsWait,
	}, Reads("event stream subscription; consumes nothing"))
}

func (a *Adapter) handleEventsWait(ctx context.Context, args map[string]any) (any, error) {
	if a.client == nil {
		return nil, toolError("daemon_unavailable", "mux_events_wait requires muxd daemon routing; start muxd and run mux mcp against that catalog")
	}

	scopes, err := decodeEventScopes(args)
	if err != nil {
		return nil, err
	}
	kinds := splitCSVArg(args, "kind")

	waitMS := intArg(args, "wait_ms", 5000)
	if waitMS <= 0 {
		waitMS = 5000
	}
	maxEvents := intArg(args, "max_events", 1)
	if maxEvents <= 0 {
		maxEvents = 1
	}
	if maxEvents > 100 {
		maxEvents = 100
	}

	streamCtx, cancel := context.WithTimeout(ctx, time.Duration(waitMS)*time.Millisecond)
	defer cancel()

	stream, errCh, err := a.client.StreamEvents(streamCtx, client.EventsStreamQuery{
		SinceSeq:  int64(intArg(args, "since_seq", 0)),
		Scopes:    scopes,
		Kinds:     kinds,
		SessionID: str(args, "session_id"),
	})
	if err != nil {
		return nil, classifyClientErr(err, "")
	}

	out := make([]map[string]any, 0, maxEvents)
	var lastSeq int64
	for len(out) < maxEvents {
		select {
		case ev, ok := <-stream:
			if !ok {
				stream = nil
				if len(out) >= maxEvents {
					break
				}
				goto done
			}
			if ev.Seq > lastSeq {
				lastSeq = ev.Seq
			}
			out = append(out, map[string]any{
				"seq":          ev.Seq,
				"kind":         ev.Kind,
				"scope":        ev.Scope,
				"session_id":   ev.SessionID,
				"payload_json": ev.PayloadJSON,
			})
		case err := <-errCh:
			if err != nil && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
				return nil, toolError("internal_error", err.Error())
			}
			goto done
		case <-streamCtx.Done():
			goto done
		}
	}

done:
	return toolJSON(map[string]any{
		"ok":             true,
		"events":         out,
		"count":          len(out),
		"timed_out":      errors.Is(streamCtx.Err(), context.DeadlineExceeded),
		"next_since_seq": lastSeq,
	}), nil
}

func decodeEventScopesAllowEmpty(args map[string]any) ([]events.Scope, error) {
	raw := splitCSVArg(args, "scope")
	if len(raw) == 0 {
		return nil, nil
	}
	scopes := make([]events.Scope, 0, len(raw))
	for _, rawScope := range raw {
		scope := rawScope
		switch scope {
		case events.ScopeDaemon, events.ScopeSession, events.ScopeBroker:
			scopes = append(scopes, scope)
		default:
			return nil, toolError("invalid_request", "scope must be daemon, session, or broker")
		}
	}
	return scopes, nil
}

func decodeEventScopes(args map[string]any) ([]string, error) {
	typed, err := decodeEventScopesAllowEmpty(args)
	if err != nil {
		return nil, err
	}
	if len(typed) == 0 {
		return []string{events.ScopeDaemon}, nil
	}
	scopes := make([]string, 0, len(typed))
	scopes = append(scopes, typed...)
	return scopes, nil
}

func splitCSVArg(args map[string]any, key string) []string {
	raw := str(args, key)
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func stringScopes(scopes []events.Scope) []string {
	if len(scopes) == 0 {
		return nil
	}
	out := make([]string, 0, len(scopes))
	out = append(out, scopes...)
	return out
}
