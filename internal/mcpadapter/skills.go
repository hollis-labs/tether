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
	), a.handleSkillList)
	a.addTool(s, mcp.NewTool("mux_skill_get",
		mcp.WithDescription("Load a Tether skill by id and return its instructions. Use when a boot prompt lists a skill pointer like `/refactor-go`; pass `refactor-go` as skill_id, then follow the returned body."),
		mcp.WithString("skill_id", mcp.Required(), mcp.Description("Skill id from the boot prompt, with or without the leading slash")),
	), a.handleSkill)
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

func resolveSkillForTool(catalogRoot, id string) (skills.Skill, string, error) {
	workingDir, _ := os.Getwd()
	s, err := skills.ResolveLayered(catalogRoot, workingDir, id)
	if err != nil {
		return skills.Skill{}, "", err
	}
	return s.Skill, s.Layer, nil
}

func discoverSkillsForTool(catalogRoot string) ([]skills.LayeredSkill, error) {
	workingDir, _ := os.Getwd()
	return skills.DiscoverLayered(catalogRoot, workingDir)
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

func validateSkillLookupID(id string) error {
	if id == "" {
		return fmt.Errorf("skill_id required")
	}
	if id == "." || id == ".." || filepath.Base(id) != id || strings.ContainsAny(id, `/\`) {
		return fmt.Errorf("skill_id must be a skill id, not a path")
	}
	return nil
}
