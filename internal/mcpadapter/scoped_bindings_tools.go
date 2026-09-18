// Package mcpadapter — scoped_bindings_tools.go wires the native
// tether_registry_scoped_binding_* MCP tools (T08, messaging vNext),
// giving MCP callers parity with T04's scoped role/slot binding
// primitive -- CLI parity landed the same task as
// `mux registry scoped-bindings`.
package mcpadapter

import (
	"context"
	"errors"

	gomcp "github.com/hollis-labs/go-mcp/server"

	"github.com/hollis-labs/tether/internal/registry"
)

// registerScopedBindingsTools wires the tether_registry_scoped_binding_*
// native tools onto s. set requires registry.write; resolve/revisions
// are read-only (same-host UDS trust, matching the rest of this package).
func (a *Adapter) registerScopedBindingsTools(s *gomcp.Server) {
	a.addTool(s, gomcp.Tool{
		Name: "tether_registry_scoped_binding_set",
		Description: "Publish a new revision for a consumer-owned (scope, slot) role binding, " +
			"e.g. scope='run-42' slot='reviewer'. Tether does not interpret scope/slot " +
			"names or grant command authority from a binding. Requires the registry.write scope.",
		InputSchema: gomcp.ObjectSchema(map[string]any{
			"scope":       strProp("Consumer-owned scope, e.g. a run or team id."),
			"slot":        strProp("Role/slot name within the scope, e.g. 'reviewer'."),
			"target_urns": strArrProp("One or more target URNs for this slot."),
			"created_by":  strProp("Caller URN recorded as provenance for this revision."),
		}, "scope", "slot", "target_urns", "created_by"),
		Handler: a.handleScopedBindingSet,
	}, Writes())

	a.addTool(s, gomcp.Tool{
		Name: "tether_registry_scoped_binding_resolve",
		Description: "Resolve the current revision for (scope, slot). single=true resolves to " +
			"exactly one target, erroring (conflict) on zero or multiple targets rather " +
			"than making the caller guess. Read-only; no scope required.",
		InputSchema: gomcp.ObjectSchema(map[string]any{
			"scope":  strProp("Consumer-owned scope."),
			"slot":   strProp("Role/slot name within the scope."),
			"single": boolProp("Resolve to exactly one target (default false: return every target)."),
		}, "scope", "slot"),
		Handler: a.handleScopedBindingResolve,
	}, Reads("scoped binding resolution"))

	a.addTool(s, gomcp.Tool{
		Name:        "tether_registry_scoped_binding_revisions",
		Description: "List every revision ever published for (scope, slot), newest first. Read-only; no scope required.",
		InputSchema: gomcp.ObjectSchema(map[string]any{
			"scope": strProp("Consumer-owned scope."),
			"slot":  strProp("Role/slot name within the scope."),
		}, "scope", "slot"),
		Handler: a.handleScopedBindingRevisions,
	}, Reads("scoped binding revision history"))
}

func (a *Adapter) handleScopedBindingSet(ctx context.Context, args map[string]any) (any, error) {
	if err := a.checkScope(ScopeRegistryWrite); err != nil {
		return nil, err
	}
	scope := str(args, "scope")
	slot := str(args, "slot")
	createdBy := str(args, "created_by")
	targets := stringSliceArg(args, "target_urns")
	if scope == "" || slot == "" || createdBy == "" || len(targets) == 0 {
		return nil, toolError("invalid_request", "scope, slot, created_by, and at least one target_urns entry are required")
	}
	if a.client == nil {
		return nil, toolError("internal_error", "tether_registry_scoped_binding_set requires daemon routing; start MCP with mux mcp")
	}
	out, err := a.client.ScopedBindings().Set(ctx, scope, slot, targets, nil, createdBy)
	if err != nil {
		if isDaemonUnreachable(err) {
			return nil, daemonUnreachableError(err)
		}
		return nil, mapScopedBindingErr(err)
	}
	return toolJSON(map[string]any{"ok": true, "binding": out}), nil
}

func (a *Adapter) handleScopedBindingResolve(ctx context.Context, args map[string]any) (any, error) {
	scope := str(args, "scope")
	slot := str(args, "slot")
	if scope == "" || slot == "" {
		return nil, toolError("invalid_request", "scope and slot are required")
	}
	if a.client == nil {
		return nil, toolError("internal_error", "tether_registry_scoped_binding_resolve requires daemon routing; start MCP with mux mcp")
	}
	if boolArg(args, "single") {
		target, binding, err := a.client.ScopedBindings().ResolveSingle(ctx, scope, slot)
		if err != nil {
			if isDaemonUnreachable(err) {
				return nil, daemonUnreachableError(err)
			}
			return nil, mapScopedBindingErr(err)
		}
		return toolJSON(map[string]any{"ok": true, "target_urn": target, "binding": binding}), nil
	}
	out, err := a.client.ScopedBindings().Resolve(ctx, scope, slot)
	if err != nil {
		if isDaemonUnreachable(err) {
			return nil, daemonUnreachableError(err)
		}
		return nil, mapScopedBindingErr(err)
	}
	return toolJSON(map[string]any{"ok": true, "binding": out}), nil
}

func (a *Adapter) handleScopedBindingRevisions(ctx context.Context, args map[string]any) (any, error) {
	scope := str(args, "scope")
	slot := str(args, "slot")
	if scope == "" || slot == "" {
		return nil, toolError("invalid_request", "scope and slot are required")
	}
	if a.client == nil {
		return nil, toolError("internal_error", "tether_registry_scoped_binding_revisions requires daemon routing; start MCP with mux mcp")
	}
	out, err := a.client.ScopedBindings().ListRevisions(ctx, scope, slot)
	if err != nil {
		if isDaemonUnreachable(err) {
			return nil, daemonUnreachableError(err)
		}
		return nil, mapScopedBindingErr(err)
	}
	if out == nil {
		out = []registry.ScopedBinding{}
	}
	return toolJSON(map[string]any{"ok": true, "revisions": out}), nil
}

// mapScopedBindingErr converts a scoped-binding client error to an MCP
// tool-error envelope.
func mapScopedBindingErr(err error) error {
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
