package mcpadapter

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	gomcp "github.com/hollis-labs/go-mcp/server"

	"github.com/hollis-labs/tether/internal/skills"
)

func (a *Adapter) registerSkillTools(s *gomcp.Server) {
	a.addTool(s, gomcp.Tool{
		Name:        "mux_skill_list",
		Description: "List Tether skills visible through layered discovery. Returns id, name, description, triggers, path, and layer; use mux_skill_get to load a skill body.",
		InputSchema: gomcp.EmptyObjectSchema(),
		Handler:     a.handleSkillList,
	}, Reads("skill catalog listing"))
	a.addTool(s, gomcp.Tool{
		Name:        "mux_skill_broker",
		Description: "Return ranked skill recommendations for a specific task, role, project, or trigger set. This is the progressive-discovery companion to mux_skill_list: it returns metadata, reasons, and ranking, then the caller uses mux_skill_get for the chosen skill body.",
		InputSchema: gomcp.InputSchema(
			gomcp.StringProp("query", "Free-text task or intent, for example 'refactor handler' or 'capture findings'", false),
			gomcp.StringProp("role", "Optional requester role signal, for example 'backend'", false),
			gomcp.StringProp("project", "Optional project signal, for example 'nanite'", false),
			gomcp.StringProp("task_id", "Optional Torque task id for forward-compatible enrichment; v1 does not dereference it in-process", false),
			gomcp.StringProp("triggers", "Optional comma-separated preferred trigger terms, for example 'refactor,cleanup'", false),
			gomcp.StringProp("layers", "Optional comma-separated layer filter, for example 'project,user'", false),
			gomcp.NumberProp("limit", "Optional max results, default 5, max 20", false),
		),
		Handler: a.handleSkillBroker,
	}, Reads("skills.BrokerLayered reads the catalog and cwd; selection only"))
	a.addTool(s, gomcp.Tool{
		Name:        "mux_skill_get",
		Description: "Load a Tether skill by id and return its instructions. Use when a boot prompt lists a skill pointer like `/refactor-go`; pass `refactor-go` as skill_id, then follow the returned body.",
		InputSchema: gomcp.InputSchema(
			gomcp.StringProp("skill_id", "Skill id from the boot prompt, with or without the leading slash", true),
		),
		Handler: a.handleSkill,
	}, Reads("skill catalog file read"))
}

func (a *Adapter) handleSkillList(_ context.Context, _ map[string]any) (any, error) {
	if a.svc == nil || strings.TrimSpace(a.svc.CatalogRoot) == "" {
		return nil, toolError("internal_error", "catalog root is not configured")
	}
	all, err := discoverSkillsForTool(a.svc.CatalogRoot)
	if err != nil {
		return nil, toolError("internal_error", err.Error())
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

func (a *Adapter) handleSkill(_ context.Context, args map[string]any) (any, error) {
	id := normalizeSkillID(str(args, "skill_id"))
	if err := validateSkillLookupID(id); err != nil {
		return nil, toolError("invalid_request", err.Error())
	}
	if a.svc == nil || strings.TrimSpace(a.svc.CatalogRoot) == "" {
		return nil, toolError("internal_error", "catalog root is not configured")
	}

	skill, layer, err := resolveSkillForTool(a.svc.CatalogRoot, id)
	if err != nil {
		return nil, toolError("not_found", err.Error())
	}
	resp := skillMetadataJSON(skills.LayeredSkill{Skill: skill, Layer: layer})
	resp["ok"] = true
	resp["body"] = skill.Body
	return toolJSON(resp), nil
}

func (a *Adapter) handleSkillBroker(_ context.Context, args map[string]any) (any, error) {
	if a.svc == nil || strings.TrimSpace(a.svc.CatalogRoot) == "" {
		return nil, toolError("internal_error", "catalog root is not configured")
	}

	result, err := skills.BrokerLayered(a.svc.CatalogRoot, currentWorkingDir(), skills.BrokerQuery{
		Query:    str(args, "query"),
		Role:     str(args, "role"),
		Project:  str(args, "project"),
		TaskID:   str(args, "task_id"),
		Triggers: csvArg(args, "triggers"),
		Layers:   csvArg(args, "layers"),
		Limit:    intArg(args, "limit", 5),
	})
	if err != nil {
		return nil, toolError("internal_error", err.Error())
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
				"query":    str(args, "query"),
				"role":     str(args, "role"),
				"project":  str(args, "project"),
				"task_id":  str(args, "task_id"),
				"triggers": csvArg(args, "triggers"),
				"layers":   csvArg(args, "layers"),
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

func csvArg(args map[string]any, key string) []string {
	raw := str(args, key)
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
