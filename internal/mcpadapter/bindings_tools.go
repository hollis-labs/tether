// Package mcpadapter — bindings_tools.go wires the native
// `tether_registry_binding_*` MCP tools (T08, messaging vNext), giving
// MCP callers parity with T07's HTTP-only /registry/bindings surface
// (internal/api/bindings.go) — CLI parity landed the same task under
// `mux registry bindings ...` (cmd/mux/registry.go).
//
// Every handler routes through a.client, matching the daemon-routing
// convention established for registry/group tools in this same task
// (see registry_tools.go's package doc).
package mcpadapter

import (
	"context"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/hollis-labs/tether/internal/registry"
)

// registerBindingsTools wires the tether_registry_binding_* native tools
// onto s. lease/renew/revoke require registry.write; current/list are
// read-only (same-host UDS trust, matching the rest of this package).
func (a *Adapter) registerBindingsTools(s *server.MCPServer) {
	a.addTool(s, mcp.NewTool("tether_registry_binding_lease",
		mcp.WithDescription(
			"Lease a published-local (pull-only) RuntimeBinding for a target URN -- the "+
				"external bridge registration surface T07 defines. Always mints "+
				"visibility='published-local'; capabilities must be exactly [\"pull-only\"]. "+
				"Refuses (conflict) to supersede a binding Tether itself manages "+
				"(private-local/tether-hosted). Requires the registry.write scope.",
		),
		mcp.WithString("target_urn", mcp.Required(), mcp.Description("msg:// target URN (session or agent).")),
		mcp.WithString("session_id", mcp.Required(), mcp.Description("Self-asserted session id the caller is leasing on behalf of.")),
		mcp.WithString("host_id", mcp.Required(), mcp.Description("Identifier for the external host/bridge process.")),
		mcp.WithString("attempt_id", mcp.Required(), mcp.Description("Identifier for this specific lease attempt.")),
		mcp.WithArray("capabilities", mcp.Required(), mcp.Description(`Must be exactly ["pull-only"].`)),
		mcp.WithNumber("ttl_seconds", mcp.Description("Lease duration in seconds; 0 or omitted means no expiry.")),
	), a.handleBindingLease)

	a.addTool(s, mcp.NewTool("tether_registry_binding_renew",
		mcp.WithDescription("Extend an existing binding's lease. Fails (conflict) if a newer generation now exists for the same target. Requires the registry.write scope."),
		mcp.WithString("binding_id", mcp.Required(), mcp.Description("Binding id returned by a prior lease.")),
		mcp.WithNumber("ttl_seconds", mcp.Description("New lease duration in seconds; 0 or omitted means no expiry.")),
	), a.handleBindingRenew)

	a.addTool(s, mcp.NewTool("tether_registry_binding_revoke",
		mcp.WithDescription("Relinquish a binding lease. Idempotent. Requires the registry.write scope."),
		mcp.WithString("binding_id", mcp.Required(), mcp.Description("Binding id to revoke.")),
	), a.handleBindingRevoke)

	a.addTool(s, mcp.NewTool("tether_registry_binding_current",
		mcp.WithDescription("Get the authoritative current binding for a target -- the highest-generation, non-revoked, non-expired binding. Read-only; no scope required."),
		mcp.WithString("target_urn", mcp.Required(), mcp.Description("msg:// target URN.")),
	), a.handleBindingCurrent)

	a.addTool(s, mcp.NewTool("tether_registry_binding_list",
		mcp.WithDescription("List every binding ever leased for a target, newest generation first (audit view). Read-only; no scope required."),
		mcp.WithString("target_urn", mcp.Required(), mcp.Description("msg:// target URN.")),
	), a.handleBindingList)
}

// ─── handlers ─────────────────────────────────────────────────────────────────

func (a *Adapter) handleBindingLease(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if errRes := a.checkScope(ScopeRegistryWrite); errRes != nil {
		return errRes, nil
	}
	targetURN := str(req, "target_urn")
	sessionID := str(req, "session_id")
	hostID := str(req, "host_id")
	attemptID := str(req, "attempt_id")
	if targetURN == "" || sessionID == "" || hostID == "" || attemptID == "" {
		return toolError("invalid_request", "target_urn, session_id, host_id, and attempt_id are required"), nil
	}
	caps := stringSliceArg(req, "capabilities")
	if a.client == nil {
		return toolError("internal_error", "tether_registry_binding_lease requires daemon routing; start MCP with mux mcp"), nil
	}
	out, err := a.client.Bindings().Lease(ctx, targetURN, sessionID, hostID, attemptID, caps, intArg(req, "ttl_seconds", 0))
	if err != nil {
		if isDaemonUnreachable(err) {
			return daemonUnreachableError(err), nil
		}
		return mapRegistryErr(err), nil
	}
	return toolJSON(map[string]any{"ok": true, "binding": out}), nil
}

func (a *Adapter) handleBindingRenew(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if errRes := a.checkScope(ScopeRegistryWrite); errRes != nil {
		return errRes, nil
	}
	bindingID := str(req, "binding_id")
	if bindingID == "" {
		return toolError("invalid_request", "binding_id is required"), nil
	}
	if a.client == nil {
		return toolError("internal_error", "tether_registry_binding_renew requires daemon routing; start MCP with mux mcp"), nil
	}
	out, err := a.client.Bindings().Renew(ctx, bindingID, intArg(req, "ttl_seconds", 0))
	if err != nil {
		if isDaemonUnreachable(err) {
			return daemonUnreachableError(err), nil
		}
		return mapRegistryErr(err), nil
	}
	return toolJSON(map[string]any{"ok": true, "binding": out}), nil
}

func (a *Adapter) handleBindingRevoke(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if errRes := a.checkScope(ScopeRegistryWrite); errRes != nil {
		return errRes, nil
	}
	bindingID := str(req, "binding_id")
	if bindingID == "" {
		return toolError("invalid_request", "binding_id is required"), nil
	}
	if a.client == nil {
		return toolError("internal_error", "tether_registry_binding_revoke requires daemon routing; start MCP with mux mcp"), nil
	}
	if err := a.client.Bindings().Revoke(ctx, bindingID); err != nil {
		if isDaemonUnreachable(err) {
			return daemonUnreachableError(err), nil
		}
		return mapRegistryErr(err), nil
	}
	return toolJSON(map[string]any{"ok": true, "binding_id": bindingID}), nil
}

func (a *Adapter) handleBindingCurrent(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	targetURN := str(req, "target_urn")
	if targetURN == "" {
		return toolError("invalid_request", "target_urn is required"), nil
	}
	if a.client == nil {
		return toolError("internal_error", "tether_registry_binding_current requires daemon routing; start MCP with mux mcp"), nil
	}
	out, err := a.client.Bindings().Current(ctx, targetURN)
	if err != nil {
		if isDaemonUnreachable(err) {
			return daemonUnreachableError(err), nil
		}
		return mapRegistryErr(err), nil
	}
	return toolJSON(map[string]any{"ok": true, "binding": out}), nil
}

func (a *Adapter) handleBindingList(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	targetURN := str(req, "target_urn")
	if targetURN == "" {
		return toolError("invalid_request", "target_urn is required"), nil
	}
	if a.client == nil {
		return toolError("internal_error", "tether_registry_binding_list requires daemon routing; start MCP with mux mcp"), nil
	}
	out, err := a.client.Bindings().ListForTarget(ctx, targetURN)
	if err != nil {
		if isDaemonUnreachable(err) {
			return daemonUnreachableError(err), nil
		}
		return mapRegistryErr(err), nil
	}
	if out == nil {
		out = []registry.RuntimeBinding{}
	}
	return toolJSON(map[string]any{"ok": true, "bindings": out}), nil
}
