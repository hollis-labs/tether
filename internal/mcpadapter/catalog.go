package mcpadapter

import (
	"context"
	"fmt"
	"sort"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/chrispian/agent-mux/internal/bootgen"
)

func (a *Adapter) registerHealthTools(s *server.MCPServer) {
	a.addTool(s, mcp.NewTool("mux_health",
		mcp.WithDescription("Health check for the agent-mux MCP adapter. Returns version and catalog summary."),
	), a.handleHealth)
}

func (a *Adapter) registerCatalogTools(s *server.MCPServer) {
	a.addTool(s, mcp.NewTool("mux_catalog_list_projects",
		mcp.WithDescription("List all projects defined in the agent-mux catalog."),
	), a.handleListProjects)

	a.addTool(s, mcp.NewTool("mux_catalog_list_agents",
		mcp.WithDescription("List all agent profiles defined in the agent-mux catalog."),
	), a.handleListAgents)

	a.addTool(s, mcp.NewTool("mux_catalog_list_providers",
		mcp.WithDescription("List all provider definitions in the agent-mux catalog."),
	), a.handleListProviders)

	a.addTool(s, mcp.NewTool("mux_catalog_list_launches",
		mcp.WithDescription("List all launch profiles in the agent-mux catalog. A launch profile combines a project, agent, and provider into a named runnable configuration."),
	), a.handleListLaunches)

	a.addTool(s, mcp.NewTool("mux_catalog_list_boot_profiles",
		mcp.WithDescription("List available boot prompt profiles from the catalog boot-profiles directory."),
	), a.handleListBootProfiles)
}

// ─── handlers ─────────────────────────────────────────────────────────────────

func (a *Adapter) handleHealth(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	cat := a.svc.Catalog
	return toolJSON(map[string]any{
		"ok":        true,
		"version":   version,
		"projects":  len(cat.Projects),
		"agents":    len(cat.Agents),
		"providers": len(cat.Providers),
		"launches":  len(cat.Launches),
	}), nil
}

func (a *Adapter) handleListProjects(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	projects := a.svc.ListProjects()
	sort.Slice(projects, func(i, j int) bool { return projects[i].ID < projects[j].ID })
	return toolJSON(map[string]any{
		"ok":       true,
		"projects": projects,
		"count":    len(projects),
	}), nil
}

func (a *Adapter) handleListAgents(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	agents := a.svc.ListAgents()
	sort.Slice(agents, func(i, j int) bool { return agents[i].ID < agents[j].ID })
	return toolJSON(map[string]any{
		"ok":     true,
		"agents": agents,
		"count":  len(agents),
	}), nil
}

func (a *Adapter) handleListProviders(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	providers := a.svc.ListProviders()
	sort.Slice(providers, func(i, j int) bool { return providers[i].ID < providers[j].ID })
	return toolJSON(map[string]any{
		"ok":        true,
		"providers": providers,
		"count":     len(providers),
	}), nil
}

func (a *Adapter) handleListLaunches(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	cat := a.svc.Catalog
	type launchBrief struct {
		ID       string `json:"id"`
		Project  string `json:"project"`
		Agent    string `json:"agent"`
		Provider string `json:"provider"`
	}
	launches := make([]launchBrief, 0, len(cat.Launches))
	for id, l := range cat.Launches {
		launches = append(launches, launchBrief{
			ID:       id,
			Project:  l.Project,
			Agent:    l.Agent,
			Provider: l.Provider,
		})
	}
	sort.Slice(launches, func(i, j int) bool { return launches[i].ID < launches[j].ID })
	return toolJSON(map[string]any{
		"ok":       true,
		"launches": launches,
		"count":    len(launches),
	}), nil
}

func (a *Adapter) handleListBootProfiles(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	profilesDir := fmt.Sprintf("%s/boot-profiles", a.svc.CatalogRoot)
	profiles, err := bootgen.LoadProfiles(profilesDir)
	if err != nil {
		return toolError("internal_error", fmt.Sprintf("load boot profiles: %v", err)), nil
	}
	type brief struct {
		ID          string `json:"id"`
		DisplayName string `json:"display_name"`
	}
	out := make([]brief, 0, len(profiles))
	for id, p := range profiles {
		out = append(out, brief{ID: id, DisplayName: p.DisplayName})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return toolJSON(map[string]any{
		"ok":       true,
		"profiles": out,
		"count":    len(out),
	}), nil
}
