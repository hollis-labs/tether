package mcpadapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
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
	Status     string              `json:"status"`
	Source     string              `json:"source"`
	ObservedAt time.Time           `json:"observed_at"`
	Validated  bool                `json:"validated"`
	Error      *catalogReadFailure `json:"error,omitempty"`
}

// catalogReadFailure is the complete error boundary for catalog reload
// responses. Category and location are selected from fixed vocabularies; the
// underlying loader or validator error is deliberately not retained because
// YAML decoder and validation messages can contain raw configuration values.
type catalogReadFailure struct {
	Category string `json:"category"`
	Location string `json:"location"`
}

func (a *Adapter) catalogForRead() (*config.Catalog, catalogReadObservation, *catalogReadFailure) {
	observation := catalogReadObservation{
		Status:     "reload_failed",
		Source:     "catalog_root",
		ObservedAt: time.Now().UTC(),
	}
	if a == nil || a.svc == nil {
		failure := &catalogReadFailure{Category: "unavailable", Location: "adapter"}
		observation.Error = failure
		return nil, observation, failure
	}

	root := strings.TrimSpace(a.svc.CatalogRoot)
	if root == "" {
		observation.Source = "startup_snapshot"
		if a.svc.Catalog == nil {
			failure := &catalogReadFailure{Category: "unavailable", Location: "catalog"}
			observation.Error = failure
			return nil, observation, failure
		}
		observation.Status = "current"
		return a.svc.Catalog, observation, nil
	}

	cat, err := config.LoadLayered(root)
	observation.ObservedAt = time.Now().UTC()
	if err != nil {
		failure := classifyCatalogLoadFailure(err)
		observation.Error = failure
		return nil, observation, failure
	}
	if failure := validateCatalogRead(cat); failure != nil {
		observation.Error = failure
		return nil, observation, failure
	}

	observation.Status = "current"
	observation.Validated = true
	return cat, observation, nil
}

func validateCatalogRead(cat *config.Catalog) *catalogReadFailure {
	if cat == nil {
		return &catalogReadFailure{Category: "unavailable", Location: "catalog"}
	}
	for _, project := range cat.Projects {
		if strings.TrimSpace(project.ID) == "" {
			return &catalogReadFailure{Category: "validation", Location: "projects.id"}
		}
	}
	for _, agent := range cat.Agents {
		if strings.TrimSpace(agent.ID) == "" {
			return &catalogReadFailure{Category: "validation", Location: "agents.id"}
		}
	}
	for _, provider := range cat.Providers {
		if strings.TrimSpace(provider.ID) == "" {
			return &catalogReadFailure{Category: "validation", Location: "providers.id"}
		}
	}
	for _, launch := range cat.Launches {
		if strings.TrimSpace(launch.ID) == "" {
			return &catalogReadFailure{Category: "validation", Location: "launches.id"}
		}
	}
	if err := cat.Validate(); err != nil {
		return &catalogReadFailure{Category: "validation", Location: catalogValidationLocation(err)}
	}
	return nil
}

func classifyCatalogLoadFailure(err error) *catalogReadFailure {
	category := "load"
	var pathErr *fs.PathError
	message := err.Error()
	switch {
	case errors.As(err, &pathErr):
		category = "io"
	case strings.Contains(message, "parse ") || strings.Contains(message, "yaml:"):
		category = "decode"
	case strings.Contains(message, "discover layered agents"):
		category = "discovery"
	}
	return &catalogReadFailure{Category: category, Location: catalogLoadLocation(message)}
}

func catalogLoadLocation(message string) string {
	normalized := strings.ReplaceAll(message, `\`, "/")
	switch {
	case strings.Contains(normalized, "global.yaml") || strings.Contains(normalized, "load global:"):
		return "global"
	case strings.Contains(normalized, "/projects/"):
		return "projects"
	case strings.Contains(normalized, "/providers/"):
		return "providers"
	case strings.Contains(normalized, "/launches/"):
		return "launches"
	case strings.Contains(normalized, "/sandbox-profiles/") || strings.Contains(normalized, "load sandbox-profiles"):
		return "sandbox_profiles"
	case strings.Contains(normalized, "/agents/") || strings.Contains(normalized, "discover layered agents"):
		return "agents"
	default:
		return "catalog"
	}
}

func catalogValidationLocation(err error) string {
	message := err.Error()
	switch {
	case strings.HasPrefix(message, "launch "):
		return "launches"
	case strings.HasPrefix(message, "provider "):
		return "providers"
	case strings.HasPrefix(message, "agent "):
		return "agents"
	case strings.HasPrefix(message, "global defaults.permission_mode"):
		return "global.catalog.defaults.permission_mode"
	case strings.HasPrefix(message, "global ai."):
		return "global.ai"
	case strings.HasPrefix(message, "federation"):
		return "global.federation"
	default:
		return "catalog"
	}
}

func catalogReloadToolError(failure *catalogReadFailure) *mcp.CallToolResult {
	body, _ := json.Marshal(map[string]any{
		"ok":      false,
		"code":    "catalog_reload_failed",
		"message": "launch catalog reload failed",
		"error":   failure,
	})
	return mcp.NewToolResultError(string(body))
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
		return catalogReloadToolError(err), nil
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
		return catalogReloadToolError(err), nil
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
		return catalogReloadToolError(err), nil
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
		return catalogReloadToolError(err), nil
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
