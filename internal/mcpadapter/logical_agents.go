package mcpadapter

import (
	"context"
	"errors"
	"sort"

	gomcp "github.com/hollis-labs/go-mcp/server"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/store"
)

func (a *Adapter) registerLogicalAgentTools(s *gomcp.Server) {
	a.addTool(s, gomcp.Tool{
		Name:        "tether_logical_agent_list",
		Description: "List all logical agents registered in the tether store. Logical agents are durable identities that persist across sessions and accumulate checkpoints.",
		InputSchema: gomcp.EmptyObjectSchema(),
		Handler:     a.handleLogicalAgentList,
	}, Reads("GET /logical-agents"))

	a.addTool(s, gomcp.Tool{
		Name:        "tether_logical_agent_resume",
		Description: "Resume a logical agent: starts a new session using its most recent checkpoint as the boot context. Requires session.write scope.",
		InputSchema: gomcp.InputSchema(
			gomcp.StringProp("logical_agent_id", "Logical agent ID", true),
			gomcp.StringProp("idempotency_key", "Optional. Makes the resume idempotent: a retry with the same key returns the session the first resume created (replayed=true) instead of starting another. Keys are one global, unauthenticated space; prefix them (e.g. \"myapp/<run>/<step>\").", false),
		),
		Handler: a.handleLogicalAgentResume,
	}, Writes())
}

// ─── handlers ─────────────────────────────────────────────────────────────────

func (a *Adapter) handleLogicalAgentList(ctx context.Context, _ map[string]any) (any, error) {
	if a.readsViaDaemon() {
		// GET /logical-agents' summary: id, name, launch_id and the
		// checkpoint policy and status.
		agents, err := a.client.ListLogicalAgents(ctx)
		if err != nil {
			return nil, daemonReadError(err, "")
		}
		sort.Slice(agents, func(i, j int) bool { return agents[i].ID < agents[j].ID })
		return toolJSON(map[string]any{
			"ok":             true,
			"logical_agents": agents,
			"count":          len(agents),
		}), nil
	}
	rows, err := a.svc.Store.ListLogicalAgents()
	if err != nil {
		return nil, toolError("internal_error", err.Error())
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	return toolJSON(map[string]any{
		"ok":             true,
		"logical_agents": rows,
		"count":          len(rows),
	}), nil
}

func (a *Adapter) handleLogicalAgentResume(ctx context.Context, args map[string]any) (any, error) {
	if denied := a.checkScope(ScopeSessionWrite); denied != nil {
		return nil, denied
	}
	id := str(args, "logical_agent_id")
	if id == "" {
		return nil, toolError("invalid_request", "logical_agent_id required")
	}
	opts := api.ResumeOptions{IdempotencyKey: str(args, "idempotency_key")}
	if a.client != nil {
		res, err := a.client.ResumeLogicalAgentWithOptions(ctx, id, opts)
		if err != nil {
			if isDaemonUnreachable(err) {
				return nil, daemonUnreachableError(err)
			}
			return nil, classifyClientErr(err, id)
		}
		return toolJSON(map[string]any{
			"ok":               true,
			"session_id":       res.ID,
			"workspace":        res.Workspace,
			"log":              res.Log,
			"provider_id":      res.ProviderID,
			"provider_kind":    res.ProviderKind,
			"logical_agent_id": res.LogicalAgentID,
			"replayed":         res.Replayed,
		}), nil
	}
	res, err := a.svc.ResumeLogicalAgent(id, opts)
	if err != nil {
		if errors.Is(err, store.ErrIdempotencyConflict) {
			return nil, toolError("idempotency_conflict", err.Error())
		}
		if isNotFound(err) {
			return nil, toolError("not_found", "logical agent not found or no checkpoint: "+id)
		}
		return nil, toolError("internal_error", err.Error())
	}
	return toolJSON(map[string]any{
		"ok":               true,
		"session_id":       res.SessionID,
		"workspace":        res.Workspace,
		"log":              res.LogPath,
		"provider_id":      res.ProviderID,
		"provider_kind":    res.ProviderKind,
		"logical_agent_id": res.LogicalAgentID,
		"replayed":         res.Replayed,
	}), nil
}
