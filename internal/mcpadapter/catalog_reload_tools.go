// Package mcpadapter — catalog_reload_tools.go wires tether_catalog_reload
// (CW-20261013-0002): make the daemon pick up catalog edits without a restart.
package mcpadapter

import (
	"context"

	gomcp "github.com/hollis-labs/libs/mcp/plugin-mcp/go-mcp/server"
)

func (a *Adapter) registerCatalogReloadTools(s *gomcp.Server) {
	a.addTool(s, gomcp.Tool{
		Name: "tether_catalog_reload",
		Description: "Make the daemon re-read its catalog from disk and use it for new launches, without a restart. The catalog is validated first; one that does not load or validate is refused and nothing changes. " +
			"Agents, projects, providers, launches and sandbox profiles are swapped in. Running sessions keep what they started with. Settings in global.yaml that the daemon reads once at start (listener, identity, AI providers, MCP gateway, modules) are not swapped: the report lists them under restart_required. " +
			"Returns what was added, removed and changed per kind. Requires the catalog.write scope.",
		InputSchema: gomcp.EmptyObjectSchema(),
		Handler:     a.handleCatalogReload,
	}, Writes())
}

func (a *Adapter) handleCatalogReload(ctx context.Context, _ map[string]any) (any, error) {
	if err := a.checkScope(ScopeCatalogWrite); err != nil {
		return nil, err
	}
	if a.client == nil {
		return nil, toolError("internal_error", "tether_catalog_reload requires daemon routing; start MCP with tether mcp")
	}
	report, err := a.client.ReloadCatalog(ctx)
	if err != nil {
		if isDaemonUnreachable(err) {
			return nil, daemonUnreachableError(err)
		}
		return nil, toolError("catalog_reload_refused", err.Error())
	}
	return toolJSON(report), nil
}
