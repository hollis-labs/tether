package mcpadapter

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/hollis-labs/tether/internal/bootgen"
)

func (a *Adapter) registerBootTools(s *server.MCPServer) {
	a.addTool(s, mcp.NewTool("mux_boot_generate",
		mcp.WithDescription("Generate a boot prompt for an agent by profile ID. The boot prompt assembles slot content from static files, role summaries, skill indexes, shell commands, and HTTP endpoints as defined in the profile YAML. Pipe the output to a CLI tool or capture it for an API provider."),
		mcp.WithString("profile_id", mcp.Required(), mcp.Description("Boot profile ID (see mux_catalog_list_boot_profiles)")),
	), a.handleBootGenerate)
}

// ─── handlers ─────────────────────────────────────────────────────────────────

func (a *Adapter) handleBootGenerate(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	profileID := str(req, "profile_id")
	if profileID == "" {
		return toolError("invalid_request", "profile_id required"), nil
	}

	profilesDir := filepath.Join(a.svc.CatalogRoot, "boot-profiles")
	profiles, err := bootgen.LoadProfiles(profilesDir)
	if err != nil {
		return toolError("internal_error", fmt.Sprintf("load boot profiles: %v", err)), nil
	}

	p, ok := profiles[profileID]
	if !ok {
		return toolError("not_found", fmt.Sprintf("boot profile %q not found", profileID)), nil
	}

	var buf bytes.Buffer
	if err := bootgen.Generate(ctx, p, a.svc.CatalogRoot, &buf); err != nil {
		return toolError("internal_error", fmt.Sprintf("generate boot prompt: %v", err)), nil
	}

	return toolJSON(map[string]any{
		"ok":          true,
		"profile_id":  profileID,
		"boot_prompt": buf.String(),
	}), nil
}
