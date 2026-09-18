package mcpadapter

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"

	gomcp "github.com/hollis-labs/go-mcp/server"

	"github.com/hollis-labs/tether/internal/bootgen"
)

func (a *Adapter) registerBootTools(s *gomcp.Server) {
	a.addTool(s, gomcp.Tool{
		Name:        "mux_boot_generate",
		Description: "Generate a boot prompt for an agent by profile ID. The boot prompt assembles slot content from static files, role summaries, skill indexes, shell commands, and HTTP endpoints as defined in the profile YAML. Pipe the output to a CLI tool or capture it for an API provider.",
		InputSchema: gomcp.InputSchema(
			gomcp.StringProp("profile_id", "Boot profile ID (see mux_catalog_list_boot_profiles)", true),
		),
		Handler: a.handleBootGenerate,
	}, Reads("bootgen.Generate renders to an io.Writer and creates no file"))
}

// ─── handlers ─────────────────────────────────────────────────────────────────

func (a *Adapter) handleBootGenerate(ctx context.Context, args map[string]any) (any, error) {
	profileID := str(args, "profile_id")
	if profileID == "" {
		return nil, toolError("invalid_request", "profile_id required")
	}

	profilesDir := filepath.Join(a.svc.CatalogRoot, "boot-profiles")
	profiles, err := bootgen.LoadProfiles(profilesDir)
	if err != nil {
		return nil, toolError("internal_error", fmt.Sprintf("load boot profiles: %v", err))
	}

	p, ok := profiles[profileID]
	if !ok {
		return nil, toolError("not_found", fmt.Sprintf("boot profile %q not found", profileID))
	}

	var buf bytes.Buffer
	if err := bootgen.Generate(ctx, p, a.svc.CatalogRoot, &buf); err != nil {
		return nil, toolError("internal_error", fmt.Sprintf("generate boot prompt: %v", err))
	}

	return toolJSON(map[string]any{
		"ok":          true,
		"profile_id":  profileID,
		"boot_prompt": buf.String(),
	}), nil
}
