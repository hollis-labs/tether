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

	gomcp "github.com/hollis-labs/go-mcp/server"

	"github.com/hollis-labs/tether/internal/registry"
)

// registerBindingsTools wires the tether_registry_binding_* native tools
// onto s. lease/renew/revoke require registry.write; current/list are
// read-only (same-host UDS trust, matching the rest of this package).
func (a *Adapter) registerBindingsTools(s *gomcp.Server) {
	a.addTool(s, gomcp.Tool{
		Name: "tether_registry_binding_lease",
		Description: "Lease a published-local (pull-only) RuntimeBinding for a target URN -- the " +
			"external bridge registration surface T07 defines. Always mints " +
			"visibility='published-local'; capabilities must be exactly [\"pull-only\"]. " +
			"Refuses (conflict) to supersede a binding Tether itself manages " +
			"(private-local/tether-hosted). Requires the registry.write scope.",
		InputSchema: gomcp.InputSchema(
			gomcp.StringProp("target_urn", "msg:// target URN (session or agent).", true),
			gomcp.StringProp("session_id", "Self-asserted session id the caller is leasing on behalf of.", true),
			gomcp.StringProp("host_id", "Identifier for the external host/bridge process.", true),
			gomcp.StringProp("attempt_id", "Identifier for this specific lease attempt.", true),
			gomcp.ArrayProp("capabilities", `Must be exactly ["pull-only"].`, true, nil),
			gomcp.NumberProp("ttl_seconds", "Lease duration in seconds; 0 or omitted means no expiry.", false),
		),
		Handler: a.handleBindingLease,
	}, Writes())

	a.addTool(s, gomcp.Tool{
		Name:        "tether_registry_binding_renew",
		Description: "Extend an existing binding's lease. Fails (conflict) if a newer generation now exists for the same target. Requires the registry.write scope.",
		InputSchema: gomcp.InputSchema(
			gomcp.StringProp("binding_id", "Binding id returned by a prior lease.", true),
			gomcp.NumberProp("ttl_seconds", "New lease duration in seconds; 0 or omitted means no expiry.", false),
		),
		Handler: a.handleBindingRenew,
	}, Writes())

	a.addTool(s, gomcp.Tool{
		Name:        "tether_registry_binding_revoke",
		Description: "Relinquish a binding lease. Idempotent. Requires the registry.write scope.",
		InputSchema: gomcp.InputSchema(
			gomcp.StringProp("binding_id", "Binding id to revoke.", true),
		),
		Handler: a.handleBindingRevoke,
	}, Destroys("revokes the lease; the holder stops receiving without being told"))

	a.addTool(s, gomcp.Tool{
		Name:        "tether_registry_binding_current",
		Description: "Get the authoritative current binding for a target -- the highest-generation, non-revoked, non-expired binding. Read-only; no scope required.",
		InputSchema: gomcp.InputSchema(
			gomcp.StringProp("target_urn", "msg:// target URN.", true),
		),
		Handler: a.handleBindingCurrent,
	}, Reads("current binding lookup"))

	a.addTool(s, gomcp.Tool{
		Name:        "tether_registry_binding_list",
		Description: "List every binding ever leased for a target, newest generation first (audit view). Read-only; no scope required.",
		InputSchema: gomcp.InputSchema(
			gomcp.StringProp("target_urn", "msg:// target URN.", true),
		),
		Handler: a.handleBindingList,
	}, Reads("binding listing"))
}

// ─── handlers ─────────────────────────────────────────────────────────────────

func (a *Adapter) handleBindingLease(ctx context.Context, args map[string]any) (any, error) {
	if err := a.checkScope(ScopeRegistryWrite); err != nil {
		return nil, err
	}
	targetURN := str(args, "target_urn")
	sessionID := str(args, "session_id")
	hostID := str(args, "host_id")
	attemptID := str(args, "attempt_id")
	if targetURN == "" || sessionID == "" || hostID == "" || attemptID == "" {
		return nil, toolError("invalid_request", "target_urn, session_id, host_id, and attempt_id are required")
	}
	caps := stringSliceArg(args, "capabilities")
	if a.client == nil {
		return nil, toolError("internal_error", "tether_registry_binding_lease requires daemon routing; start MCP with mux mcp")
	}
	out, err := a.client.Bindings().Lease(ctx, targetURN, sessionID, hostID, attemptID, caps, intArg(args, "ttl_seconds", 0))
	if err != nil {
		if isDaemonUnreachable(err) {
			return nil, daemonUnreachableError(err)
		}
		return nil, mapRegistryErr(err)
	}
	return toolJSON(map[string]any{"ok": true, "binding": out}), nil
}

func (a *Adapter) handleBindingRenew(ctx context.Context, args map[string]any) (any, error) {
	if err := a.checkScope(ScopeRegistryWrite); err != nil {
		return nil, err
	}
	bindingID := str(args, "binding_id")
	if bindingID == "" {
		return nil, toolError("invalid_request", "binding_id is required")
	}
	if a.client == nil {
		return nil, toolError("internal_error", "tether_registry_binding_renew requires daemon routing; start MCP with mux mcp")
	}
	out, err := a.client.Bindings().Renew(ctx, bindingID, intArg(args, "ttl_seconds", 0))
	if err != nil {
		if isDaemonUnreachable(err) {
			return nil, daemonUnreachableError(err)
		}
		return nil, mapRegistryErr(err)
	}
	return toolJSON(map[string]any{"ok": true, "binding": out}), nil
}

func (a *Adapter) handleBindingRevoke(ctx context.Context, args map[string]any) (any, error) {
	if err := a.checkScope(ScopeRegistryWrite); err != nil {
		return nil, err
	}
	bindingID := str(args, "binding_id")
	if bindingID == "" {
		return nil, toolError("invalid_request", "binding_id is required")
	}
	if a.client == nil {
		return nil, toolError("internal_error", "tether_registry_binding_revoke requires daemon routing; start MCP with mux mcp")
	}
	if err := a.client.Bindings().Revoke(ctx, bindingID); err != nil {
		if isDaemonUnreachable(err) {
			return nil, daemonUnreachableError(err)
		}
		return nil, mapRegistryErr(err)
	}
	return toolJSON(map[string]any{"ok": true, "binding_id": bindingID}), nil
}

func (a *Adapter) handleBindingCurrent(ctx context.Context, args map[string]any) (any, error) {
	targetURN := str(args, "target_urn")
	if targetURN == "" {
		return nil, toolError("invalid_request", "target_urn is required")
	}
	if a.client == nil {
		return nil, toolError("internal_error", "tether_registry_binding_current requires daemon routing; start MCP with mux mcp")
	}
	out, err := a.client.Bindings().Current(ctx, targetURN)
	if err != nil {
		if isDaemonUnreachable(err) {
			return nil, daemonUnreachableError(err)
		}
		return nil, mapRegistryErr(err)
	}
	return toolJSON(map[string]any{"ok": true, "binding": out}), nil
}

func (a *Adapter) handleBindingList(ctx context.Context, args map[string]any) (any, error) {
	targetURN := str(args, "target_urn")
	if targetURN == "" {
		return nil, toolError("invalid_request", "target_urn is required")
	}
	if a.client == nil {
		return nil, toolError("internal_error", "tether_registry_binding_list requires daemon routing; start MCP with mux mcp")
	}
	out, err := a.client.Bindings().ListForTarget(ctx, targetURN)
	if err != nil {
		if isDaemonUnreachable(err) {
			return nil, daemonUnreachableError(err)
		}
		return nil, mapRegistryErr(err)
	}
	if out == nil {
		out = []registry.RuntimeBinding{}
	}
	return toolJSON(map[string]any{"ok": true, "bindings": out}), nil
}
