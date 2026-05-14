package mcpadapter

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/skills"
)

func (a *Adapter) registerSkillTools(s *server.MCPServer) {
	a.addTool(s, mcp.NewTool("Skill",
		mcp.WithDescription("Load a Tether skill by id and return its instructions. Use when a boot prompt lists a skill pointer like `/refactor-go`; pass `refactor-go` as skill_id, then follow the returned body."),
		mcp.WithString("skill_id", mcp.Required(), mcp.Description("Skill id from the boot prompt, without the leading slash")),
	), a.handleSkill)
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
	return toolJSON(map[string]any{
		"ok":          true,
		"id":          skill.ID,
		"name":        skill.Name,
		"description": skill.Description,
		"triggers":    skill.Triggers,
		"body":        skill.Body,
		"path":        skill.Path,
		"layer":       layer,
	}), nil
}

func resolveSkillForTool(catalogRoot, id string) (skills.Skill, string, error) {
	workingDir, _ := os.Getwd()
	cat, err := config.Discover(config.DefaultLayers(catalogRoot, workingDir))
	if err != nil {
		return skills.Skill{}, "", fmt.Errorf("discover skills: %w", err)
	}
	if lp, ok := cat.SkillPaths[id]; ok {
		s, err := skills.ParseFile(lp.Path)
		if err != nil {
			return skills.Skill{}, "", fmt.Errorf("skill %q: %w", id, err)
		}
		return s, lp.Layer.String(), nil
	}

	for _, candidate := range fallbackSkillPaths(id) {
		if _, err := os.Stat(candidate.path); err != nil {
			continue
		}
		s, err := skills.ParseFile(candidate.path)
		if err != nil {
			return skills.Skill{}, "", fmt.Errorf("skill %q: %w", id, err)
		}
		return s, candidate.layer, nil
	}

	return skills.Skill{}, "", fmt.Errorf("skill %q not found in catalog or legacy skill directories", id)
}

func fallbackSkillPaths(id string) []struct {
	layer string
	path  string
} {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return []struct {
		layer string
		path  string
	}{
		{layer: "user-tether", path: filepath.Join(home, ".tether", "skills", id+".md")},
		{layer: "legacy-nanite", path: filepath.Join(home, ".nanite", "skills", id+".md")},
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
