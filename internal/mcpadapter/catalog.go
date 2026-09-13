package mcpadapter

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/hollis-labs/tether/internal/bootgen"
	"github.com/hollis-labs/tether/internal/config"
)

// catalogReadObservation describes the catalog value used for one MCP
// response. Production adapters reload from CatalogRoot for every read so a
// long-running MCP process observes valid on-disk edits without replacing the
// Service catalog used by runtime factories and launch ownership.
type catalogReadObservation struct {
	Status     string    `json:"status"`
	Source     string    `json:"source"`
	ObservedAt time.Time `json:"observed_at"`
	Validated  bool      `json:"validated"`
	Error      string    `json:"error,omitempty"`
}

func (a *Adapter) catalogForRead() (*config.Catalog, catalogReadObservation, error) {
	observation := catalogReadObservation{
		Status:     "reload_failed",
		Source:     "catalog_root",
		ObservedAt: time.Now().UTC(),
	}
	if a == nil || a.svc == nil {
		err := fmt.Errorf("catalog service is unavailable")
		observation.Error = err.Error()
		return nil, observation, err
	}

	root := strings.TrimSpace(a.svc.CatalogRoot)
	if root == "" {
		observation.Source = "startup_snapshot"
		if a.svc.Catalog == nil {
			err := fmt.Errorf("catalog is unavailable")
			observation.Error = err.Error()
			return nil, observation, err
		}
		observation.Status = "current"
		return a.svc.Catalog, observation, nil
	}

	cat, err := config.LoadLayered(root)
	if err == nil {
		err = cat.Validate()
	}
	observation.ObservedAt = time.Now().UTC()
	if err != nil {
		err = fmt.Errorf("reload launch catalog: %w", err)
		observation.Error = err.Error()
		return nil, observation, err
	}

	observation.Status = "current"
	observation.Validated = true
	return cat, observation, nil
}

func (a *Adapter) registerHealthTools(s *server.MCPServer) {
	a.addTool(s, mcp.NewTool("mux_health",
		mcp.WithDescription("Health check for the agent-mux MCP adapter. Returns version and catalog summary."),
	), Reads("runtime observation and validated launch catalog reload; no store or catalog write"), a.handleHealth)
}

func (a *Adapter) registerCatalogTools(s *server.MCPServer) {
	a.addTool(s, mcp.NewTool("mux_catalog_list_projects",
		mcp.WithDescription("List all projects defined in the agent-mux catalog."),
	), Reads("validated launch catalog reload and project listing"), a.handleListProjects)

	a.addTool(s, mcp.NewTool("mux_catalog_list_agents",
		mcp.WithDescription("List all agent profiles defined in the agent-mux catalog."),
	), Reads("validated layered launch catalog reload and agent listing"), a.handleListAgents)

	a.addTool(s, mcp.NewTool("mux_catalog_list_providers",
		mcp.WithDescription("List all provider definitions in the agent-mux catalog."),
	), Reads("validated launch catalog reload and provider listing"), a.handleListProviders)

	a.addTool(s, mcp.NewTool("mux_catalog_list_launches",
		mcp.WithDescription("List all launch profiles in the agent-mux catalog. A launch profile combines a project, agent, and provider into a named runnable configuration."),
	), Reads("validated layered launch catalog reload and launch listing"), a.handleListLaunches)

	a.addTool(s, mcp.NewTool("mux_catalog_list_boot_profiles",
		mcp.WithDescription("List available boot prompt profiles from the catalog boot-profiles directory."),
	), Reads("catalog boot-profiles directory listing"), a.handleListBootProfiles)
}

// ─── handlers ─────────────────────────────────────────────────────────────────

func (a *Adapter) handleHealth(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	cat, catalogRead, catalogErr := a.catalogForRead()
	payload := map[string]any{
		"ok":           catalogErr == nil,
		"version":      a.runtime.Build.Version,
		"runtime":      a.runtime,
		"catalog_read": catalogRead,
	}
	if catalogErr == nil {
		payload["projects"] = len(cat.Projects)
		payload["agents"] = len(cat.Agents)
		payload["providers"] = len(cat.Providers)
		payload["launches"] = len(cat.Launches)
	}
	if a.upstreams != nil {
		statuses := a.upstreams.StatusSummary()
		absent := unavailableServers(statuses)
		payload["ok"] = catalogErr == nil && len(absent) == 0
		payload["upstream_servers"] = statuses
		payload["unavailable_servers"] = absent
		payload["health_basis"] = "observed connections; no active liveness probe"
	}
	return toolJSON(payload), nil
}

func (a *Adapter) handleListProjects(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	cat, catalogRead, err := a.catalogForRead()
	if err != nil {
		return toolError("catalog_reload_failed", err.Error()), nil
	}
	projects := make([]config.Project, 0, len(cat.Projects))
	for _, project := range cat.Projects {
		projects = append(projects, project)
	}
	sort.Slice(projects, func(i, j int) bool { return projects[i].ID < projects[j].ID })
	return toolJSON(map[string]any{
		"ok":           true,
		"projects":     projects,
		"count":        len(projects),
		"catalog_read": catalogRead,
	}), nil
}

func (a *Adapter) handleListAgents(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	cat, catalogRead, err := a.catalogForRead()
	if err != nil {
		return toolError("catalog_reload_failed", err.Error()), nil
	}
	agents := make([]config.Agent, 0, len(cat.Agents))
	for _, agent := range cat.Agents {
		agents = append(agents, agent)
	}
	sort.Slice(agents, func(i, j int) bool { return agents[i].ID < agents[j].ID })
	return toolJSON(map[string]any{
		"ok":           true,
		"agents":       agents,
		"count":        len(agents),
		"catalog_read": catalogRead,
	}), nil
}

func (a *Adapter) handleListProviders(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	cat, catalogRead, err := a.catalogForRead()
	if err != nil {
		return toolError("catalog_reload_failed", err.Error()), nil
	}
	providers := make([]config.Provider, 0, len(cat.Providers))
	for _, provider := range cat.Providers {
		providers = append(providers, provider)
	}
	sort.Slice(providers, func(i, j int) bool { return providers[i].ID < providers[j].ID })
	return toolJSON(map[string]any{
		"ok":           true,
		"providers":    providers,
		"count":        len(providers),
		"catalog_read": catalogRead,
	}), nil
}

func (a *Adapter) handleListLaunches(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	cat, catalogRead, err := a.catalogForRead()
	if err != nil {
		return toolError("catalog_reload_failed", err.Error()), nil
	}
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
		"ok":           true,
		"launches":     launches,
		"count":        len(launches),
		"catalog_read": catalogRead,
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
