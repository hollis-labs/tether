package mcpadapter

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/hollis-labs/tether/internal/agentops"
	"github.com/hollis-labs/tether/internal/config"
)

// registerAgentOpsTools wires the agent catalog-ops surface: list/show (read,
// no scope) and create/edit (write, gated by catalog.write). These mirror the
// `mux agents` CLI so an agent can manage agent definitions over MCP.
func (a *Adapter) registerAgentOpsTools(s *server.MCPServer) {
	a.addTool(s, mcp.NewTool("mux_agent_list",
		mcp.WithDescription("List all agents across the system, user, and project discovery layers. Each entry is annotated with the layer it resolved from and its file path. Read-only; no scope required."),
	), a.handleAgentList)

	a.addTool(s, mcp.NewTool("mux_agent_show",
		mcp.WithDescription("Show one agent's full resolved definition, including which discovery layer it came from and its file path. Read-only; no scope required."),
		mcp.WithString("id", mcp.Required(), mcp.Description("Agent ID")),
	), a.handleAgentShow)

	a.addTool(s, mcp.NewTool("mux_agent_create",
		mcp.WithDescription("Create a new agent YAML in a discovery layer. Requires the catalog.write scope.\n\nScope controls where the agent file is written and which launches can see it:\n  project (default) — <repo>/.tether/agents/; visible to that repo's launches only; commit it with the repo. Requires the 'project' argument.\n  user              — ~/.tether/agents/; visible to all of this machine's launches.\n  system            — the shared system catalog.\n\nPrefer project scope for repo-specific agents (auditors, builders for one codebase)."),
		mcp.WithString("id", mcp.Required(), mcp.Description("Agent ID — a single name with no path separators; becomes the YAML filename. Kebab-case recommended.")),
		mcp.WithString("scope", mcp.Description("Discovery layer: project (default) | user | system.")),
		mcp.WithString("project", mcp.Description("Catalog project ID — required when scope=project. The agent is written to that project's repo at <repo_root>/.tether/agents/.")),
		mcp.WithString("name", mcp.Description("Human-readable name (defaults to id).")),
		mcp.WithString("roles", mcp.Description("Comma-separated role list (optional).")),
		mcp.WithString("skills", mcp.Description("Comma-separated skill ID list (optional).")),
		mcp.WithString("system_prompt", mcp.Description("Agent system prompt (optional).")),
		mcp.WithString("agent_prompt", mcp.Description("Agent persona prompt (optional).")),
	), a.handleAgentCreate)

	a.addTool(s, mcp.NewTool("mux_agent_edit",
		mcp.WithDescription("Update an existing agent's fields in place, in whichever discovery layer it currently resides. Requires the catalog.write scope.\n\nOnly the arguments you pass are changed; omitted arguments are left as-is. Passing roles/skills replaces the existing list — pass an empty string to clear it. Scalar fields (name/system_prompt/agent_prompt) cannot be cleared to empty via edit. Note: edit rewrites the file in canonical YAML form, so comments and any unknown fields in the original file are not preserved."),
		mcp.WithString("id", mcp.Required(), mcp.Description("Agent ID to edit.")),
		mcp.WithString("name", mcp.Description("New human-readable name (optional).")),
		mcp.WithString("roles", mcp.Description("Comma-separated role list — replaces existing roles; empty string clears them (optional).")),
		mcp.WithString("skills", mcp.Description("Comma-separated skill ID list — replaces existing skills; empty string clears them (optional).")),
		mcp.WithString("system_prompt", mcp.Description("New system prompt (optional).")),
		mcp.WithString("agent_prompt", mcp.Description("New persona prompt (optional).")),
	), a.handleAgentEdit)
}

// ─── handlers ─────────────────────────────────────────────────────────────────

func (a *Adapter) handleAgentList(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	cat, errRes := a.discoverAgents()
	if errRes != nil {
		return errRes, nil
	}
	type brief struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Layer string `json:"layer"`
		Path  string `json:"path"`
	}
	out := make([]brief, 0, len(cat.Agents))
	for _, la := range cat.Agents {
		out = append(out, brief{
			ID:    la.Agent.ID,
			Name:  la.Agent.Name,
			Layer: la.Layer.String(),
			Path:  la.Path,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return toolJSON(map[string]any{"ok": true, "agents": out, "count": len(out)}), nil
}

func (a *Adapter) handleAgentShow(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id := str(req, "id")
	if id == "" {
		return toolError("invalid_argument", "id is required"), nil
	}
	cat, errRes := a.discoverAgents()
	if errRes != nil {
		return errRes, nil
	}
	la, ok := cat.Agents[id]
	if !ok {
		return toolError("not_found", fmt.Sprintf("agent %q not found in any discovery layer", id)), nil
	}
	return toolJSON(map[string]any{
		"ok":    true,
		"layer": la.Layer.String(),
		"path":  la.Path,
		"agent": la.Agent,
	}), nil
}

func (a *Adapter) handleAgentCreate(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if errRes := a.checkScope(ScopeCatalogWrite); errRes != nil {
		return errRes, nil
	}
	id := str(req, "id")
	if !agentops.ValidID(id) {
		return toolError("invalid_argument", "id is required and must be a single name with no path separators"), nil
	}
	layer, err := agentops.ParseScope(str(req, "scope"))
	if err != nil {
		return toolError("invalid_argument", err.Error()), nil
	}
	root, errRes := a.layerRoot(layer, str(req, "project"))
	if errRes != nil {
		return errRes, nil
	}
	path, err := agentops.Create(root, id, agentops.Params{
		Name:         str(req, "name"),
		Roles:        csvArgPresent(req, "roles"),
		Skills:       csvArgPresent(req, "skills"),
		SystemPrompt: str(req, "system_prompt"),
		AgentPrompt:  str(req, "agent_prompt"),
	})
	if err != nil {
		// An already-exists is a caller error (conflict); anything else
		// (permission denied, bad layer root, write failure) is internal.
		if errors.Is(err, agentops.ErrExists) {
			return toolError("conflict", err.Error()), nil
		}
		return toolError("internal_error", err.Error()), nil
	}
	return toolJSON(map[string]any{
		"ok":    true,
		"id":    id,
		"layer": layer.String(),
		"path":  path,
	}), nil
}

func (a *Adapter) handleAgentEdit(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if errRes := a.checkScope(ScopeCatalogWrite); errRes != nil {
		return errRes, nil
	}
	id := str(req, "id")
	if id == "" {
		return toolError("invalid_argument", "id is required"), nil
	}
	cat, errRes := a.discoverAgents()
	if errRes != nil {
		return errRes, nil
	}
	la, ok := cat.Agents[id]
	if !ok {
		return toolError("not_found", fmt.Sprintf("agent %q not found in any discovery layer", id)), nil
	}
	updated, err := agentops.Update(la.Path, agentops.Params{
		Name:         str(req, "name"),
		Roles:        csvArgPresent(req, "roles"),
		Skills:       csvArgPresent(req, "skills"),
		SystemPrompt: str(req, "system_prompt"),
		AgentPrompt:  str(req, "agent_prompt"),
	})
	if err != nil {
		return toolError("internal_error", err.Error()), nil
	}
	return toolJSON(map[string]any{
		"ok":    true,
		"layer": la.Layer.String(),
		"path":  la.Path,
		"agent": updated,
	}), nil
}

// ─── helpers ──────────────────────────────────────────────────────────────────

// discoverAgents runs layered agent discovery (system + user + project)
// anchored at the catalog root and the adapter's working directory. It
// returns an MCP error result instead of an error so handlers can return it
// directly.
func (a *Adapter) discoverAgents() (*config.LayeredCatalog, *mcp.CallToolResult) {
	if a.svc == nil || strings.TrimSpace(a.svc.CatalogRoot) == "" {
		return nil, toolError("internal_error", "catalog root is not configured")
	}
	cat, err := config.Discover(config.DefaultLayers(a.svc.CatalogRoot, currentWorkingDir()))
	if err != nil {
		return nil, toolError("internal_error", fmt.Sprintf("agent discovery: %v", err))
	}
	return cat, nil
}

// layerRoot resolves the on-disk root directory for the given discovery
// layer. For the project layer, projectID names a catalog project and the
// agent is written into that project's repo (<repo_root>/.tether).
func (a *Adapter) layerRoot(layer config.Layer, projectID string) (string, *mcp.CallToolResult) {
	switch layer {
	case config.LayerSystem:
		return config.Expand(a.svc.CatalogRoot), nil
	case config.LayerUser:
		home, err := os.UserHomeDir()
		if err != nil {
			return "", toolError("internal_error", fmt.Sprintf("resolve home dir: %v", err))
		}
		return filepath.Join(home, ".tether"), nil
	case config.LayerProject:
		if projectID == "" {
			return "", toolError("invalid_argument", "scope=project requires the 'project' argument (a catalog project ID)")
		}
		proj, ok := a.svc.Catalog.Projects[projectID]
		if !ok {
			return "", toolError("not_found", fmt.Sprintf("unknown project %q", projectID))
		}
		if strings.TrimSpace(proj.RepoRoot) == "" {
			return "", toolError("invalid_argument", fmt.Sprintf("project %q has no repo_root; cannot place a project-scoped agent", projectID))
		}
		return filepath.Join(config.Expand(proj.RepoRoot), ".tether"), nil
	default:
		return "", toolError("invalid_argument", fmt.Sprintf("unsupported layer %q", layer))
	}
}

// csvArgPresent reads a comma-separated list argument with presence
// semantics — distinct from csvArg in skills.go, which is nil-on-empty. A
// missing key returns nil, which agentops.Params treats as "leave unchanged".
// A key that is present (even as an empty string) returns a non-nil slice,
// so a caller can deliberately clear Roles/Skills to an empty list. Trimmed,
// empty elements are dropped.
func csvArgPresent(req mcp.CallToolRequest, key string) []string {
	raw, ok := req.GetArguments()[key]
	if !ok {
		return nil
	}
	s, _ := raw.(string)
	out := []string{}
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
