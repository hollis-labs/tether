package mcpadapter

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	gomcp "github.com/hollis-labs/go-mcp/server"

	"github.com/hollis-labs/tether/internal/agentops"
	"github.com/hollis-labs/tether/internal/config"
)

// registerAgentOpsTools wires the agent catalog-ops surface: list/show (read,
// no scope) and create/edit (write, gated by catalog.write). These mirror the
// `mux agents` CLI so an agent can manage agent definitions over MCP.
func (a *Adapter) registerAgentOpsTools(s *gomcp.Server) {
	a.addTool(s, gomcp.Tool{
		Name:        "mux_agent_list",
		Description: "List all agents across the system, user, and project discovery layers. Each entry is annotated with the layer it resolved from and its file path. Read-only; no scope required.",
		InputSchema: gomcp.EmptyObjectSchema(),
		Handler:     a.handleAgentList,
	}, Reads("catalog agent listing"))

	a.addTool(s, gomcp.Tool{
		Name:        "mux_agent_show",
		Description: "Show one agent's full resolved definition, including which discovery layer it came from and its file path. Read-only; no scope required.",
		InputSchema: gomcp.ObjectSchema(map[string]any{
			"id": strProp("Agent ID"),
		}, "id"),
		Handler: a.handleAgentShow,
	}, Reads("catalog agent lookup"))

	a.addTool(s, gomcp.Tool{
		Name:        "mux_agent_create",
		Description: "Create a new agent YAML in a discovery layer. Requires the catalog.write scope.\n\nScope controls where the agent file is written and which launches can see it:\n  project (default) — <repo>/.tether/agents/; visible to that repo's launches only; commit it with the repo. Requires the 'project' argument.\n  user              — ~/.tether/agents/; visible to all of this machine's launches.\n  system            — the shared system catalog.\n\nPrefer project scope for repo-specific agents (auditors, builders for one codebase).",
		InputSchema: gomcp.ObjectSchema(map[string]any{
			"id":            strProp("Agent ID — a single name with no path separators; becomes the YAML filename. Kebab-case recommended."),
			"scope":         strProp("Discovery layer: project (default) | user | system."),
			"project":       strProp("Catalog project ID — required when scope=project. The agent is written to that project's repo at <repo_root>/.tether/agents/."),
			"name":          strProp("Human-readable name (defaults to id)."),
			"roles":         strProp("Comma-separated role list (optional)."),
			"skills":        strProp("Comma-separated skill ID list (optional)."),
			"system_prompt": strProp("Agent system prompt (optional)."),
			"agent_prompt":  strProp("Agent persona prompt (optional)."),
		}, "id"),
		Handler: a.handleAgentCreate,
	}, Writes())

	a.addTool(s, gomcp.Tool{
		Name:        "mux_agent_edit",
		Description: "Update an existing agent's fields in place, in whichever discovery layer it currently resides. Requires the catalog.write scope.\n\nOnly the arguments you pass are changed; omitted arguments are left as-is. Passing roles/skills replaces the existing list — pass an empty string to clear it. Scalar fields (name/system_prompt/agent_prompt) cannot be cleared to empty via edit. Note: edit rewrites the file in canonical YAML form, so comments and any unknown fields in the original file are not preserved.",
		InputSchema: gomcp.ObjectSchema(map[string]any{
			"id":            strProp("Agent ID to edit."),
			"name":          strProp("New human-readable name (optional)."),
			"roles":         strProp("Comma-separated role list — replaces existing roles; empty string clears them (optional)."),
			"skills":        strProp("Comma-separated skill ID list — replaces existing skills; empty string clears them (optional)."),
			"system_prompt": strProp("New system prompt (optional)."),
			"agent_prompt":  strProp("New persona prompt (optional)."),
		}, "id"),
		Handler: a.handleAgentEdit,
	}, Writes())
}

// ─── handlers ─────────────────────────────────────────────────────────────────

func (a *Adapter) handleAgentList(_ context.Context, _ map[string]any) (any, error) {
	cat, err := a.discoverAgents()
	if err != nil {
		return nil, err
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

func (a *Adapter) handleAgentShow(_ context.Context, args map[string]any) (any, error) {
	id := str(args, "id")
	if id == "" {
		return nil, toolError("invalid_argument", "id is required")
	}
	cat, err := a.discoverAgents()
	if err != nil {
		return nil, err
	}
	la, ok := cat.Agents[id]
	if !ok {
		return nil, toolError("not_found", fmt.Sprintf("agent %q not found in any discovery layer", id))
	}
	return toolJSON(map[string]any{
		"ok":    true,
		"layer": la.Layer.String(),
		"path":  la.Path,
		"agent": la.Agent,
	}), nil
}

func (a *Adapter) handleAgentCreate(_ context.Context, args map[string]any) (any, error) {
	if err := a.checkScope(ScopeCatalogWrite); err != nil {
		return nil, err
	}
	id := str(args, "id")
	if !agentops.ValidID(id) {
		return nil, toolError("invalid_argument", "id is required and must be a single name with no path separators")
	}
	layer, err := agentops.ParseScope(str(args, "scope"))
	if err != nil {
		return nil, toolError("invalid_argument", err.Error())
	}
	root, err := a.layerRoot(layer, str(args, "project"))
	if err != nil {
		return nil, err
	}
	path, err := agentops.Create(root, id, agentops.Params{
		Name:         str(args, "name"),
		Roles:        csvArgPresent(args, "roles"),
		Skills:       csvArgPresent(args, "skills"),
		SystemPrompt: str(args, "system_prompt"),
		AgentPrompt:  str(args, "agent_prompt"),
	})
	if err != nil {
		// An already-exists is a caller error (conflict); anything else
		// (permission denied, bad layer root, write failure) is internal.
		if errors.Is(err, agentops.ErrExists) {
			return nil, toolError("conflict", err.Error())
		}
		return nil, toolError("internal_error", err.Error())
	}
	return toolJSON(map[string]any{
		"ok":    true,
		"id":    id,
		"layer": layer.String(),
		"path":  path,
	}), nil
}

func (a *Adapter) handleAgentEdit(_ context.Context, args map[string]any) (any, error) {
	if err := a.checkScope(ScopeCatalogWrite); err != nil {
		return nil, err
	}
	id := str(args, "id")
	if id == "" {
		return nil, toolError("invalid_argument", "id is required")
	}
	cat, err := a.discoverAgents()
	if err != nil {
		return nil, err
	}
	la, ok := cat.Agents[id]
	if !ok {
		return nil, toolError("not_found", fmt.Sprintf("agent %q not found in any discovery layer", id))
	}
	updated, err := agentops.Update(la.Path, agentops.Params{
		Name:         str(args, "name"),
		Roles:        csvArgPresent(args, "roles"),
		Skills:       csvArgPresent(args, "skills"),
		SystemPrompt: str(args, "system_prompt"),
		AgentPrompt:  str(args, "agent_prompt"),
	})
	if err != nil {
		return nil, toolError("internal_error", err.Error())
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
// returns a *budget.ToolError-satisfying error instead of a bare error so
// handlers can return it directly.
func (a *Adapter) discoverAgents() (*config.LayeredCatalog, error) {
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
func (a *Adapter) layerRoot(layer config.Layer, projectID string) (string, error) {
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
func csvArgPresent(args map[string]any, key string) []string {
	raw, ok := args[key]
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
