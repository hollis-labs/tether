// Package mcpadapter — whoami_tools.go wires the native tether_whoami
// MCP tool (T08, messaging vNext), giving MCP callers parity with GET
// /whoami (internal/api/whoami.go) -- CLI parity landed the same task as
// `mux whoami`.
package mcpadapter

import (
	"context"

	gomcp "github.com/hollis-labs/go-mcp/server"
)

// registerWhoamiTools wires tether_whoami onto s. Read-only; no scope
// (same-host UDS trust, ADR 0045 -- as is a self-asserted claim).
func (a *Adapter) registerWhoamiTools(s *gomcp.Server) {
	a.addTool(s, gomcp.Tool{
		Name: "tether_whoami",
		Description: "Self-discovery: the registered Profile (if any), attached external-id " +
			"mappings, group memberships, and the current RuntimeBinding (host/session " +
			"currently owning delivery), if any, for the claimed URN. An unregistered " +
			"or never-bound identity is not an error -- every field is independently " +
			"best-effort.",
		InputSchema: gomcp.ObjectSchema(map[string]any{
			"as": strProp("msg:// URN to look up (self-asserted, no verification)."),
		}, "as"),
		Handler: a.handleWhoami,
	}, Reads("resolves the caller identity; asserts nothing"))
}

func (a *Adapter) handleWhoami(ctx context.Context, args map[string]any) (any, error) {
	as := str(args, "as")
	if as == "" {
		return nil, toolError("invalid_request", "as is required")
	}
	if a.client == nil {
		return nil, toolError("internal_error", "tether_whoami requires daemon routing; start MCP with mux mcp")
	}
	out, err := a.client.Whoami(ctx, as)
	if err != nil {
		if isDaemonUnreachable(err) {
			return nil, daemonUnreachableError(err)
		}
		return nil, mapRegistryErr(err)
	}
	return toolJSON(map[string]any{"ok": true, "whoami": out}), nil
}
