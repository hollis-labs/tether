// Package mcpadapter — whoami_tools.go wires the native tether_whoami
// MCP tool (T08, messaging vNext), giving MCP callers parity with GET
// /whoami (internal/api/whoami.go) -- CLI parity landed the same task as
// `mux whoami`.
package mcpadapter

import (
	"context"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// registerWhoamiTools wires tether_whoami onto s. Read-only; no scope
// (same-host UDS trust, ADR 0045 -- as is a self-asserted claim).
func (a *Adapter) registerWhoamiTools(s *server.MCPServer) {
	a.addTool(s, mcp.NewTool("tether_whoami",
		mcp.WithDescription(
			"Self-discovery: the registered Profile (if any), attached external-id "+
				"mappings, group memberships, and the current RuntimeBinding (host/session "+
				"currently owning delivery), if any, for the claimed URN. An unregistered "+
				"or never-bound identity is not an error -- every field is independently "+
				"best-effort.",
		),
		mcp.WithString("as", mcp.Required(), mcp.Description("msg:// URN to look up (self-asserted, no verification).")),
	), a.handleWhoami)
}

func (a *Adapter) handleWhoami(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	as := str(req, "as")
	if as == "" {
		return toolError("invalid_request", "as is required"), nil
	}
	if a.client == nil {
		return toolError("internal_error", "tether_whoami requires daemon routing; start MCP with mux mcp"), nil
	}
	out, err := a.client.Whoami(ctx, as)
	if err != nil {
		if isDaemonUnreachable(err) {
			return daemonUnreachableError(err), nil
		}
		return mapRegistryErr(err), nil
	}
	return toolJSON(map[string]any{"ok": true, "whoami": out}), nil
}
