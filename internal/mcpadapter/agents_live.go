package mcpadapter

import (
	"context"
	"fmt"
	"strings"

	gomcp "github.com/hollis-labs/libs/mcp/plugin-mcp/go-mcp/server"

	"github.com/hollis-labs/tether/internal/agentlive"
	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/client"
)

func (a *Adapter) registerAgentsLiveTools(s *gomcp.Server) {
	a.addTool(s, gomcp.Tool{
		Name: "tether_agents_live",
		Description: "List every live agent in one read: Tether sessions (running/detached) and kit-booted seats (external kit-session registrations plus mail-watcher and runtime-binding presence). " +
			"Rows carry agent {urn,name,ref}, kind (tether_session|kit_seat), host, state (awake|sleeping|unknown), raw_state (busy|idle when known), last_activity_at (null when never observed) and last_activity (summary), session_id/run_id, workstream_id, presence_at/presence_source and stale. " +
			"stale=true when no presence evidence was seen within stale_after_seconds (default 600); stale rows are hidden unless include_stale=true. Unknown observations stay unknown/null. Read-only; same as GET /agents/live.",
		InputSchema: gomcp.InputSchema(
			gomcp.StringProp("host", "Only rows on this host (machine name; outposts use the worker label)", false),
			gomcp.StringProp("kind", "tether_session or kit_seat", false),
			gomcp.BooleanProp("include_stale", "Include rows with no recent presence (default false)", false),
			gomcp.NumberProp("stale_after_seconds", "Presence freshness threshold in seconds, 1..86400 (default 600)", false),
			gomcp.NumberProp("limit", "Max rows, 1..1000 (default 200); the response says truncated and gives next_cursor when more exist", false),
			gomcp.StringProp("cursor", "Opaque next_cursor from a previous response", false),
		),
		Handler: a.handleAgentsLive,
	}, Reads("GET /agents/live; svc.AgentsLive"))
}

func (a *Adapter) handleAgentsLive(ctx context.Context, args map[string]any) (any, error) {
	get := func(k string) string {
		switch v := args[k].(type) {
		case nil:
			return ""
		case string:
			return strings.TrimSpace(v)
		default:
			return fmt.Sprint(v)
		}
	}
	q, err := api.ParseAgentsLiveQuery(get)
	if err != nil {
		return nil, toolError("invalid_request", err.Error())
	}
	if a.readsViaDaemon() {
		res, err := a.client.AgentsLive(ctx, client.AgentsLiveOptions{Host: q.Host, Kind: q.Kind, IncludeStale: q.IncludeStale,
			StaleAfterSeconds: int(q.StaleAfter.Seconds()), Limit: q.Limit, Cursor: get("cursor")})
		if err != nil {
			return nil, daemonReadError(err, "")
		}
		return toolJSON(agentsLiveOut(res)), nil
	}
	if a.svc == nil {
		return nil, toolError("daemon_unavailable", "agents live needs the daemon")
	}
	res, err := a.svc.AgentsLive(ctx, q, nil)
	if err != nil {
		return nil, toolError("internal_error", err.Error())
	}
	return toolJSON(agentsLiveOut(res)), nil
}

func agentsLiveOut(r agentlive.Response) map[string]any {
	return map[string]any{"ok": true, "agents": r.Agents, "count": r.Count, "limit": r.Limit, "truncated": r.Truncated, "next_cursor": r.NextCursor,
		"observed_at": r.ObservedAt, "stale_after_seconds": r.StaleAfterSeconds}
}
