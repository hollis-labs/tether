package mcpadapter

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/hollis-labs/tether/internal/skills"
)

func (a *Adapter) registerSkillTools(s *server.MCPServer) {
	a.addTool(s, mcp.NewTool("mux_skill_list",
		mcp.WithDescription("List Tether skills visible through layered discovery. Returns id, name, description, triggers, path, and layer; use mux_skill_get to load a skill body."),
	), Reads("skill catalog listing"), a.handleSkillList)
	a.addTool(s, mcp.NewTool("mux_skill_broker",
		mcp.WithDescription("Return ranked skill recommendations for a specific task, role, project, or trigger set. This is the progressive-discovery companion to mux_skill_list: it returns metadata, reasons, and ranking, then the caller uses mux_skill_get for the chosen skill body."),
		mcp.WithString("query", mcp.Description("Free-text task or intent, for example 'refactor handler' or 'capture findings'")),
		mcp.WithString("role", mcp.Description("Optional requester role signal, for example 'backend'")),
		mcp.WithString("project", mcp.Description("Optional project signal, for example 'nanite'")),
		mcp.WithString("task_id", mcp.Description("Optional Torque task id for forward-compatible enrichment; v1 does not dereference it in-process")),
		mcp.WithString("triggers", mcp.Description("Optional comma-separated preferred trigger terms, for example 'refactor,cleanup'")),
		mcp.WithString("layers", mcp.Description("Optional comma-separated layer filter, for example 'project,user'")),
		mcp.WithNumber("limit", mcp.Description("Optional max results, default 5, max 20")),
	), Reads("skills.BrokerLayered reads the catalog and cwd; selection only"), a.handleSkillBroker)
	a.addTool(s, mcp.NewTool("mux_skill_get",
		mcp.WithDescription("Load a Tether skill by id and return its instructions. Use when a boot prompt lists a skill pointer like `/refactor-go`; pass `refactor-go` as skill_id, then follow the returned body."),
		mcp.WithString("skill_id", mcp.Required(), mcp.Description("Skill id from the boot prompt, with or without the leading slash")),
	), Reads("skill catalog file read"), a.handleSkill)
}

func (a *Adapter) handleSkillList(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if a.svc == nil || strings.TrimSpace(a.svc.CatalogRoot) == "" {
		return toolError("internal_error", "catalog root is not configured"), nil
	}
	all, err := discoverSkillsForTool(a.svc.CatalogRoot)
	if err != nil {
		return toolError("internal_error", err.Error()), nil
	}
	items := make([]map[string]any, 0, len(all))
	for _, s := range all {
		items = append(items, skillMetadataJSON(s))
	}
	return toolJSON(map[string]any{
		"ok":    true,
		"items": items,
		"meta": map[string]any{
			"returned": len(items),
		},
	}), nil
}

func (a *Adapter) handleSkill(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id := normalizeSkillID(str(req, "skill_id"))
	if err := validateSkillLookupID(id); err != nil {
		return toolError("invalid_request", err.Error()), nil
	}
	if a.svc == nil || strings.TrimSpace(a.svc.CatalogRoot) == "" {
		return toolError("internal_error", "catalog root is not configured"), nil
	}

	skill, layer, err := resolveSkillForTool(a.svc.CatalogRoot, id)
	if err != nil {
		return toolError("not_found", err.Error()), nil
	}
	resp := skillMetadataJSON(skills.LayeredSkill{Skill: skill, Layer: layer})
	resp["ok"] = true
	resp["body"] = skill.Body
	return toolJSON(resp), nil
}

func (a *Adapter) handleSkillBroker(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if a.svc == nil || strings.TrimSpace(a.svc.CatalogRoot) == "" {
		return toolError("internal_error", "catalog root is not configured"), nil
	}

	result, err := skills.BrokerLayered(a.svc.CatalogRoot, currentWorkingDir(), skills.BrokerQuery{
		Query:    str(req, "query"),
		Role:     str(req, "role"),
		Project:  str(req, "project"),
		TaskID:   str(req, "task_id"),
		Triggers: csvArg(req, "triggers"),
		Layers:   csvArg(req, "layers"),
		Limit:    intArg(req, "limit", 5),
	})
	if err != nil {
		return toolError("internal_error", err.Error()), nil
	}

	items := make([]map[string]any, 0, len(result.Matches))
	for _, match := range result.Matches {
		item := skillMetadataJSON(match.LayeredSkill)
		item["score"] = map[string]any{
			"query_matches":     match.Score.QueryMatches,
			"signal_matches":    match.Score.SignalMatches,
			"preferred_matches": match.Score.PreferredMatches,
			"priority":          match.Score.Priority,
		}
		item["reasons"] = match.Reasons
		item["next"] = "mux_skill_get"
		items = append(items, item)
	}

	return toolJSON(map[string]any{
		"ok":    true,
		"items": items,
		"meta": map[string]any{
			"returned":      len(items),
			"total_visible": result.TotalVisible,
			"filters": map[string]any{
				"query":    str(req, "query"),
				"role":     str(req, "role"),
				"project":  str(req, "project"),
				"task_id":  str(req, "task_id"),
				"triggers": csvArg(req, "triggers"),
				"layers":   csvArg(req, "layers"),
			},
			"progressive_discovery": true,
			"task_context_resolved": false,
		},
	}), nil
}

func resolveSkillForTool(catalogRoot, id string) (skills.Skill, string, error) {
	workingDir, _ := os.Getwd()
	s, err := skills.ResolveLayered(catalogRoot, workingDir, id)
	if err != nil {
		return skills.Skill{}, "", err
	}
	return s.Skill, s.Layer, nil
}

func discoverSkillsForTool(catalogRoot string) ([]skills.LayeredSkill, error) {
	return skills.DiscoverLayered(catalogRoot, currentWorkingDir())
}

func skillMetadataJSON(s skills.LayeredSkill) map[string]any {
	return map[string]any{
		"id":          s.Skill.ID,
		"name":        s.Skill.Name,
		"description": s.Skill.Description,
		"triggers":    s.Skill.Triggers,
		"path":        s.Skill.Path,
		"layer":       s.Layer,
	}
}

func normalizeSkillID(id string) string {
	return strings.TrimPrefix(strings.TrimSpace(id), "/")
}

func csvArg(req mcp.CallToolRequest, key string) []string {
	raw := str(req, key)
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

func currentWorkingDir() string {
	workingDir, _ := os.Getwd()
	return workingDir
}

func validateSkillLookupID(id string) error {
	if id == "" {
		return fmt.Errorf("skill_id required")
	}
	if id == "." || id == ".." || filepath.Base(id) != id || strings.ContainsAny(id, `/\`) {
		return fmt.Errorf("skill_id must be a skill id, not a path")
	}
	return nil
}
