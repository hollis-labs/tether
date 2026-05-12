package mcpadapter

import (
	"context"
	"sort"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func (a *Adapter) registerLogicalAgentTools(s *server.MCPServer) {
	a.addTool(s, mcp.NewTool("mux_logical_agent_list",
		mcp.WithDescription("List all logical agents registered in the agent-mux store. Logical agents are durable identities that persist across sessions and accumulate checkpoints."),
	), a.handleLogicalAgentList)

	a.addTool(s, mcp.NewTool("mux_logical_agent_resume",
		mcp.WithDescription("Resume a logical agent: starts a new session using its most recent checkpoint as the boot context. Requires session.write scope."),
		mcp.WithString("logical_agent_id", mcp.Required(), mcp.Description("Logical agent ID")),
	), a.handleLogicalAgentResume)
}

// ─── handlers ─────────────────────────────────────────────────────────────────

func (a *Adapter) handleLogicalAgentList(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	rows, err := a.svc.Store.ListLogicalAgents()
	if err != nil {
		return toolError("internal_error", err.Error()), nil
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	return toolJSON(map[string]any{
		"ok":             true,
		"logical_agents": rows,
		"count":          len(rows),
	}), nil
}

func (a *Adapter) handleLogicalAgentResume(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if denied := a.checkScope(ScopeSessionWrite); denied != nil {
		return denied, nil
	}
	id := str(req, "logical_agent_id")
	if id == "" {
		return toolError("invalid_request", "logical_agent_id required"), nil
	}
	if a.client != nil {
		res, err := a.client.ResumeLogicalAgent(ctx, id)
		if err != nil {
			if isDaemonUnreachable(err) {
				return daemonUnreachableError(err), nil
			}
			return classifyClientErr(err, id), nil
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
			return toolError("not_found", "logical agent not found or no checkpoint: "+id), nil
		}
		return toolError("internal_error", err.Error()), nil
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
