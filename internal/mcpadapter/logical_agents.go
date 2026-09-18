package mcpadapter

import (
	"context"
	"sort"

	gomcp "github.com/hollis-labs/go-mcp/server"
)

func (a *Adapter) registerLogicalAgentTools(s *gomcp.Server) {
	a.addTool(s, gomcp.Tool{
		Name:        "mux_logical_agent_list",
		Description: "List all logical agents registered in the agent-mux store. Logical agents are durable identities that persist across sessions and accumulate checkpoints.",
		InputSchema: gomcp.EmptyObjectSchema(),
		Handler:     a.handleLogicalAgentList,
	}, Reads("GET /logical-agents"))

	a.addTool(s, gomcp.Tool{
		Name:        "mux_logical_agent_resume",
		Description: "Resume a logical agent: starts a new session using its most recent checkpoint as the boot context. Requires session.write scope.",
		InputSchema: gomcp.ObjectSchema(map[string]any{
			"logical_agent_id": strProp("Logical agent ID"),
		}, "logical_agent_id"),
		Handler: a.handleLogicalAgentResume,
	}, Writes())
}

// ─── handlers ─────────────────────────────────────────────────────────────────

func (a *Adapter) handleLogicalAgentList(_ context.Context, _ map[string]any) (any, error) {
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
	if a.client != nil {
		res, err := a.client.ResumeLogicalAgent(ctx, id)
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
		}), nil
	}
	res, err := a.svc.ResumeLogicalAgent(id)
	if err != nil {
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
	}), nil
}
