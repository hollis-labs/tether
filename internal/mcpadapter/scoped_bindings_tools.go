// Package mcpadapter — scoped_bindings_tools.go wires the native
// tether_registry_scoped_binding_* MCP tools (T08, messaging vNext),
// giving MCP callers parity with T04's scoped role/slot binding
// primitive -- CLI parity landed the same task as
// `mux registry scoped-bindings`.
package mcpadapter

import (
	"context"
	"errors"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/hollis-labs/tether/internal/registry"
)

// registerScopedBindingsTools wires the tether_registry_scoped_binding_*
// native tools onto s. set requires registry.write; resolve/revisions
// are read-only (same-host UDS trust, matching the rest of this package).
func (a *Adapter) registerScopedBindingsTools(s *server.MCPServer) {
	a.addTool(s, mcp.NewTool("tether_registry_scoped_binding_set",
		mcp.WithDescription(
			"Publish a new revision for a consumer-owned (scope, slot) role binding, "+
				"e.g. scope='run-42' slot='reviewer'. Tether does not interpret scope/slot "+
				"names or grant command authority from a binding. Requires the registry.write scope.",
		),
		mcp.WithString("scope", mcp.Required(), mcp.Description("Consumer-owned scope, e.g. a run or team id.")),
		mcp.WithString("slot", mcp.Required(), mcp.Description("Role/slot name within the scope, e.g. 'reviewer'.")),
		mcp.WithArray("target_urns", mcp.Required(), mcp.Description("One or more target URNs for this slot.")),
		mcp.WithString("created_by", mcp.Required(), mcp.Description("Caller URN recorded as provenance for this revision.")),
	), a.handleScopedBindingSet)

	a.addTool(s, mcp.NewTool("tether_registry_scoped_binding_resolve",
		mcp.WithDescription(
			"Resolve the current revision for (scope, slot). single=true resolves to "+
				"exactly one target, erroring (conflict) on zero or multiple targets rather "+
				"than making the caller guess. Read-only; no scope required.",
		),
		mcp.WithString("scope", mcp.Required(), mcp.Description("Consumer-owned scope.")),
		mcp.WithString("slot", mcp.Required(), mcp.Description("Role/slot name within the scope.")),
		mcp.WithBoolean("single", mcp.Description("Resolve to exactly one target (default false: return every target).")),
	), a.handleScopedBindingResolve)

	a.addTool(s, mcp.NewTool("tether_registry_scoped_binding_revisions",
		mcp.WithDescription("List every revision ever published for (scope, slot), newest first. Read-only; no scope required."),
		mcp.WithString("scope", mcp.Required(), mcp.Description("Consumer-owned scope.")),
		mcp.WithString("slot", mcp.Required(), mcp.Description("Role/slot name within the scope.")),
	), a.handleScopedBindingRevisions)
}

func (a *Adapter) handleScopedBindingSet(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if errRes := a.checkScope(ScopeRegistryWrite); errRes != nil {
		return errRes, nil
	}
	scope := str(req, "scope")
	slot := str(req, "slot")
	createdBy := str(req, "created_by")
	targets := stringSliceArg(req, "target_urns")
	if scope == "" || slot == "" || createdBy == "" || len(targets) == 0 {
		return toolError("invalid_request", "scope, slot, created_by, and at least one target_urns entry are required"), nil
	}
	if a.client == nil {
		return toolError("internal_error", "tether_registry_scoped_binding_set requires daemon routing; start MCP with mux mcp"), nil
	}
	out, err := a.client.ScopedBindings().Set(ctx, scope, slot, targets, nil, createdBy)
	if err != nil {
		if isDaemonUnreachable(err) {
			return daemonUnreachableError(err), nil
		}
		return mapScopedBindingErr(err), nil
	}
	return toolJSON(map[string]any{"ok": true, "binding": out}), nil
}

func (a *Adapter) handleScopedBindingResolve(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	scope := str(req, "scope")
	slot := str(req, "slot")
	if scope == "" || slot == "" {
		return toolError("invalid_request", "scope and slot are required"), nil
	}
	if a.client == nil {
		return toolError("internal_error", "tether_registry_scoped_binding_resolve requires daemon routing; start MCP with mux mcp"), nil
	}
	if boolArg(req, "single") {
		target, binding, err := a.client.ScopedBindings().ResolveSingle(ctx, scope, slot)
		if err != nil {
			if isDaemonUnreachable(err) {
				return daemonUnreachableError(err), nil
			}
			return mapScopedBindingErr(err), nil
		}
		return toolJSON(map[string]any{"ok": true, "target_urn": target, "binding": binding}), nil
	}
	out, err := a.client.ScopedBindings().Resolve(ctx, scope, slot)
	if err != nil {
		if isDaemonUnreachable(err) {
			return daemonUnreachableError(err), nil
		}
		return mapScopedBindingErr(err), nil
	}
	return toolJSON(map[string]any{"ok": true, "binding": out}), nil
}

func (a *Adapter) handleScopedBindingRevisions(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	scope := str(req, "scope")
	slot := str(req, "slot")
	if scope == "" || slot == "" {
		return toolError("invalid_request", "scope and slot are required"), nil
	}
	if a.client == nil {
		return toolError("internal_error", "tether_registry_scoped_binding_revisions requires daemon routing; start MCP with mux mcp"), nil
	}
	out, err := a.client.ScopedBindings().ListRevisions(ctx, scope, slot)
	if err != nil {
		if isDaemonUnreachable(err) {
			return daemonUnreachableError(err), nil
		}
		return mapScopedBindingErr(err), nil
	}
	if out == nil {
		out = []registry.ScopedBinding{}
	}
	return toolJSON(map[string]any{"ok": true, "revisions": out}), nil
}

// mapScopedBindingErr converts a scoped-binding client error to an MCP
// tool-error envelope.
func mapScopedBindingErr(err error) *mcp.CallToolResult {
	switch {
	case errors.Is(err, registry.ErrNotFound):
		return toolError("not_found", err.Error())
	case errors.Is(err, registry.ErrInvalidRequest):
		return toolError("invalid_request", err.Error())
	case errors.Is(err, registry.ErrAmbiguousBinding), errors.Is(err, registry.ErrBindingHasNoTargets):
		return toolError("conflict", err.Error())
	default:
		return toolError("internal_error", err.Error())
	}
}
