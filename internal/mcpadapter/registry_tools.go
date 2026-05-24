// Package mcpadapter — registry_tools.go wires the six native
// `tether_registry_*` MCP tools for the v0.6 federation directory service
// (T-v060-01-06). Tools dispatch directly to a.svc.Registry in-process,
// matching ADR 0034's "MCP shares the same Service composition root" rule:
// no HTTP loop back through the daemon, no parallel registry instance.
//
// Tool-prefix choice. Names use the `tether_` prefix (not `mux_`) per D14
// and the sprint spec — the directory service is the first surface where
// "Tether" is the user-facing name. Internal Go packages still say `mux`
// in places; the prefix divergence is intentional.
//
// Scope. Read tools (`lookup`, `search`) are unauthenticated — same-host
// UDS trust per D7 — and reuse the no-scope pattern from skills.go.
// Write tools (`register`, `update_self`, `deregister`, `sync`) require
// the new `registry.write` scope. Sync is classified as a write because
// it mutates the cached_at column and replaces thin-profile fields from
// the callback payload.
//
// Schema shape. mcp-go's `WithObject(name, opts...)` declares a JSON-
// schema object with `type:"object", properties:{}` and no implicit
// `additionalProperties:true` flag. That satisfies the sprint's
// "no loose additionalProperties on patches" acceptance criterion
// without an explicit per-field property map for every Profile / Patch
// scalar. The full field shape is documented in each tool's description
// so a calling agent has the schema in text form — the JSON-schema
// nested-property dance buys us no extra validation here because
// registry.Service already validates each field on dispatch.
//
// Error mapping. Service errors flow through toolError envelopes:
//
//	ErrInvalidRequest  → "invalid_request"
//	ErrNotFound        → "not_found"
//	ErrNoCallback      → success result {"ok":true,"synced":false}
//	                     (no-callback is the documented sync no-op state)
//	ErrNoResolver      → "invalid_request"   (operator misconfiguration —
//	                     matches the HTTP layer's 400 mapping)
//	ErrPayloadInvalid  → "internal_error"    (upstream callback failure)
//	ErrPayloadTooLarge → "internal_error"
//	ErrPathOutsideRoot → "internal_error"
//	ErrMintExhausted   → "internal_error"
//	(anything else)    → "internal_error"
//
// Nil-guard. When a.svc.Registry is nil (e.g. an Adapter constructed
// without a populated app.Service for a unit test) every handler returns
// an internal_error envelope rather than panicking. Mirrors the catalog/
// skills nil-guard pattern in skills.go.
package mcpadapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/hollis-labs/tether/internal/registry"
)

// ScopeRegistryWrite gates the mutating registry tools (register, update_self,
// deregister, sync). New in T-v060-01-06; deliberately separate from
// ScopeCatalogWrite so an operator can grant directory-service writes without
// granting catalog YAML writes, and vice versa.
const ScopeRegistryWrite = "registry.write"

// registerRegistryTools wires the six tether_registry_* native tools onto s.
// All six dispatch directly to *registry.Service in-process — there is no
// HTTP loop. Read tools (lookup, search) are unauthenticated; write tools
// (register, update_self, deregister, sync) require the registry.write
// scope.
func (a *Adapter) registerRegistryTools(s *server.MCPServer) {
	a.addTool(s, mcp.NewTool("tether_registry_register",
		mcp.WithDescription(
			"Register a new agent or project profile in the federation directory. "+
				"Server mints the URN (Stripe-style opaque id, e.g. agt_xxxxxxxxxx / prj_xxxxxxxxxx); "+
				"callers MUST NOT supply the urn field — doing so returns invalid_request. "+
				"The kind argument selects the entity kind. The profile argument is a JSON object "+
				"matching registry.Profile: required fields are display_name; optional fields "+
				"include title, role, description, avatar, project, status, callback, capabilities, "+
				"skills (each {name, learned_at RFC3339, optional via, level}), links (each "+
				"{kind, target}), kind_meta, host_address, health_status. "+
				"Returns the canonical Profile with the minted URN. Requires the registry.write scope.",
		),
		mcp.WithString("kind", mcp.Required(),
			mcp.Description("Entity kind: 'agent' or 'project'."),
			mcp.Enum("agent", "project"),
		),
		mcp.WithObject("profile", mcp.Required(),
			mcp.Description("Profile JSON to register. See tool description for the field shape."),
		),
	), a.handleRegistryRegister)

	a.addTool(s, mcp.NewTool("tether_registry_lookup",
		mcp.WithDescription(
			"Look up a registry profile by URN. Returns the full Profile or not_found. "+
				"Soft-deleted (status='deprecated') rows are returned by direct lookup — they "+
				"are excluded only from default Search results. Read-only; no scope required.",
		),
		mcp.WithString("urn", mcp.Required(),
			mcp.Description("Full URN as minted by Register, e.g. msg://agent/agent-mux/agt_xxxxxxxxxx."),
		),
	), a.handleRegistryLookup)

	a.addTool(s, mcp.NewTool("tether_registry_lookup_by",
		mcp.WithDescription(
			"Resolve a substrate-local external ID to one registry profile. Returns 0 or 1 row; "+
				"use this when you know a local ID like a Tether catalog slug or Cerberus owner and "+
				"want the canonical registry URN. Read-only; no scope required.",
		),
		mcp.WithString("kind", mcp.Required(),
			mcp.Description("Entity kind to resolve: 'agent', 'project', or 'group'."),
			mcp.Enum("agent", "project", "group"),
		),
		mcp.WithString("external_id", mcp.Required(),
			mcp.Description("Substrate-local identifier to resolve."),
		),
		mcp.WithString("substrate", mcp.Description("Optional substrate scope such as 'tether' or 'cerberus'.")),
	), a.handleRegistryLookupBy)

	a.addTool(s, mcp.NewTool("tether_registry_search",
		mcp.WithDescription(
			"Search the registry by filter. All filters combine with AND. Result ordering is "+
				"alphabetical on display_name. Default excludes status='deprecated'; pass "+
				"status='deprecated' to return only deprecated rows, or status='*' to return "+
				"all statuses. Read-only; no scope required.",
		),
		mcp.WithString("kind", mcp.Required(),
			mcp.Description("Entity kind to search: 'agent' or 'project'."),
			mcp.Enum("agent", "project"),
		),
		mcp.WithString("role", mcp.Description("Filter on role (exact match).")),
		mcp.WithString("title", mcp.Description("Filter on title (exact match).")),
		mcp.WithString("project", mcp.Description("Filter on project (exact match).")),
		mcp.WithString("capability", mcp.Description("Filter to rows that carry this capability string.")),
		mcp.WithString("skill_name", mcp.Description("Filter to rows that carry a skill with this name.")),
		mcp.WithString("status", mcp.Description("Filter on status. Empty → active only; 'deprecated' → deprecated only; '*' → all.")),
	), a.handleRegistrySearch)

	a.addTool(s, mcp.NewTool("tether_registry_update_self",
		mcp.WithDescription(
			"Partial-merge update of a registry row. Returns the refreshed Profile. "+
				"Scalar fields (display_name, title, role, description, avatar, project, status, "+
				"health_status, host_address, last_seen_at, kind_meta) update column-wise — only "+
				"fields present in the patch are touched. "+
				"Array fields (capabilities, skills, links) accept TWO wire shapes:\n"+
				"  (1) Shorthand `[...]` — equivalent to {mode:'replace', value:[...]}.\n"+
				"  (2) Explicit `{mode: 'replace'|'append'|'remove', value: [...]}`.\n"+
				"Empty value is always a no-op (existing arrays are not cleared). "+
				"Remove matches: capabilities by string equality; skills by name; links by (kind, target) tuple. "+
				"last_updated_by is required on every patch — caller-supplied identity string, becomes auth-bound in v060-02. "+
				"Requires the registry.write scope.",
		),
		mcp.WithString("urn", mcp.Required(),
			mcp.Description("Full URN of the row to update."),
		),
		mcp.WithObject("patch", mcp.Required(),
			mcp.Description("UpdatePatch JSON. See tool description for partial-merge semantics."),
		),
	), a.handleRegistryUpdateSelf)

	a.addTool(s, mcp.NewTool("tether_registry_deregister",
		mcp.WithDescription(
			"Soft-delete a registry row. Status flips to 'deprecated'. The row remains visible "+
				"via direct URN lookup (so callers can audit deprecated entries); default Search "+
				"excludes it. Returns the deprecated Profile. Requires the registry.write scope.",
		),
		mcp.WithString("urn", mcp.Required(),
			mcp.Description("Full URN of the row to soft-delete."),
		),
	), a.handleRegistryDeregister)

	a.addTool(s, mcp.NewTool("tether_registry_sync",
		mcp.WithDescription(
			"Refresh thin-profile columns from the row's callback URI. Returns one of two success shapes:\n"+
				"  - {ok:true, synced:false} when the row has no callback configured (no-op state).\n"+
				"  - {ok:true, synced:true, profile:<refreshed>} when the callback was invoked successfully.\n"+
				"Raw payload is NEVER stored (substrate ops-store files often contain plaintext secrets); "+
				"only the thin-profile columns + capabilities/skills/links arrays + cached_at are updated. "+
				"Requires the registry.write scope.",
		),
		mcp.WithString("urn", mcp.Required(),
			mcp.Description("Full URN of the row to sync."),
		),
	), a.handleRegistrySync)
}

// ─── handlers ─────────────────────────────────────────────────────────────────

// handleRegistryRegister services tether_registry_register. The profile
// argument arrives as a JSON object; we re-marshal it through json.Marshal
// → json.Unmarshal so registry.Profile's UnmarshalJSON applies (this is
// load-bearing for the ArrayPatch shorthand handling on capabilities/skills/
// links — though Profile uses plain []T, not ArrayPatch[T]). The map → JSON
// → struct round-trip also normalizes the time.Time fields.
func (a *Adapter) handleRegistryRegister(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if errRes := a.checkScope(ScopeRegistryWrite); errRes != nil {
		return errRes, nil
	}
	if errRes := a.requireRegistry(); errRes != nil {
		return errRes, nil
	}

	kind, errRes := requireKind(req)
	if errRes != nil {
		return errRes, nil
	}

	profile, errRes := decodeProfileArg(req, "profile")
	if errRes != nil {
		return errRes, nil
	}

	out, err := a.svc.Registry.Register(ctx, kind, profile)
	if err != nil {
		return mapRegistryErr(err), nil
	}
	return toolJSON(map[string]any{"ok": true, "profile": out}), nil
}

// handleRegistryLookup services tether_registry_lookup. Read-only; no scope.
func (a *Adapter) handleRegistryLookup(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if errRes := a.requireRegistry(); errRes != nil {
		return errRes, nil
	}
	urn := str(req, "urn")
	if urn == "" {
		return toolError("invalid_request", "urn is required"), nil
	}
	out, err := a.svc.Registry.Lookup(ctx, urn)
	if err != nil {
		return mapRegistryErr(err), nil
	}
	return toolJSON(map[string]any{"ok": true, "profile": out}), nil
}

func (a *Adapter) handleRegistryLookupBy(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if errRes := a.requireRegistry(); errRes != nil {
		return errRes, nil
	}
	kind, errRes := requireKind(req)
	if errRes != nil {
		return errRes, nil
	}
	externalID := str(req, "external_id")
	if externalID == "" {
		return toolError("invalid_request", "external_id is required"), nil
	}
	out, err := a.svc.Registry.LookupBy(ctx, kind, externalID, str(req, "substrate"))
	if err != nil {
		return mapRegistryErr(err), nil
	}
	return toolJSON(map[string]any{"ok": true, "profile": out}), nil
}

// handleRegistrySearch services tether_registry_search. Read-only; no scope.
func (a *Adapter) handleRegistrySearch(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if errRes := a.requireRegistry(); errRes != nil {
		return errRes, nil
	}
	kind, errRes := requireKind(req)
	if errRes != nil {
		return errRes, nil
	}
	f := registry.Filter{
		Role:       str(req, "role"),
		Title:      str(req, "title"),
		Project:    str(req, "project"),
		Capability: str(req, "capability"),
		SkillName:  str(req, "skill_name"),
		Status:     str(req, "status"),
	}
	out, err := a.svc.Registry.Search(ctx, kind, f)
	if err != nil {
		return mapRegistryErr(err), nil
	}
	if out == nil {
		out = []registry.Profile{}
	}
	return toolJSON(map[string]any{"ok": true, "profiles": out}), nil
}

// handleRegistryUpdateSelf services tether_registry_update_self. The patch
// argument arrives as a JSON object; re-marshal → unmarshal-into-UpdatePatch
// is load-bearing because registry.ArrayPatch.UnmarshalJSON handles the
// shorthand `[...]` vs explicit {mode,value} dispatch.
func (a *Adapter) handleRegistryUpdateSelf(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if errRes := a.checkScope(ScopeRegistryWrite); errRes != nil {
		return errRes, nil
	}
	if errRes := a.requireRegistry(); errRes != nil {
		return errRes, nil
	}
	urn := str(req, "urn")
	if urn == "" {
		return toolError("invalid_request", "urn is required"), nil
	}
	patch, errRes := decodePatchArg(req, "patch")
	if errRes != nil {
		return errRes, nil
	}
	out, err := a.svc.Registry.UpdateSelf(ctx, urn, patch)
	if err != nil {
		return mapRegistryErr(err), nil
	}
	return toolJSON(map[string]any{"ok": true, "profile": out}), nil
}

// handleRegistryDeregister services tether_registry_deregister.
func (a *Adapter) handleRegistryDeregister(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if errRes := a.checkScope(ScopeRegistryWrite); errRes != nil {
		return errRes, nil
	}
	if errRes := a.requireRegistry(); errRes != nil {
		return errRes, nil
	}
	urn := str(req, "urn")
	if urn == "" {
		return toolError("invalid_request", "urn is required"), nil
	}
	out, err := a.svc.Registry.Deregister(ctx, urn)
	if err != nil {
		return mapRegistryErr(err), nil
	}
	return toolJSON(map[string]any{"ok": true, "profile": out}), nil
}

// handleRegistrySync services tether_registry_sync. Distinct from the rest
// of the dispatch matrix because ErrNoCallback is a success result (the
// row deliberately has no callback configured — see Service.Sync's godoc),
// not an error envelope.
func (a *Adapter) handleRegistrySync(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if errRes := a.checkScope(ScopeRegistryWrite); errRes != nil {
		return errRes, nil
	}
	if errRes := a.requireRegistry(); errRes != nil {
		return errRes, nil
	}
	urn := str(req, "urn")
	if urn == "" {
		return toolError("invalid_request", "urn is required"), nil
	}
	out, err := a.svc.Registry.Sync(ctx, urn)
	if err != nil {
		if errors.Is(err, registry.ErrNoCallback) {
			return toolJSON(map[string]any{"ok": true, "synced": false}), nil
		}
		return mapRegistryErr(err), nil
	}
	return toolJSON(map[string]any{"ok": true, "synced": true, "profile": out}), nil
}

// ─── helpers ─────────────────────────────────────────────────────────────────

// requireRegistry returns a non-nil error result when a.svc or a.svc.Registry
// is nil. Mirrors the catalog/skills nil-guard so the registry tools fail
// gracefully on unconfigured Adapters.
func (a *Adapter) requireRegistry() *mcp.CallToolResult {
	if a.svc == nil || a.svc.Registry == nil {
		return toolError("internal_error", "registry service not configured")
	}
	return nil
}

// requireKind reads the kind argument and validates it against the v060-01
// vocabulary (agent + project). Service.Register/Search also validates,
// but checking here lets the error message name the argument explicitly.
func requireKind(req mcp.CallToolRequest) (registry.Kind, *mcp.CallToolResult) {
	raw := str(req, "kind")
	switch raw {
	case "agent":
		return registry.KindAgent, nil
	case "project":
		return registry.KindProject, nil
	case "group":
		return registry.KindGroup, nil
	case "":
		return "", toolError("invalid_request", "kind is required")
	default:
		return "", toolError("invalid_request", fmt.Sprintf("unsupported kind %q (expected 'agent', 'project', or 'group')", raw))
	}
}

// decodeProfileArg pulls the object argument named key, re-marshals it
// through JSON, and unmarshals into a registry.Profile. Going through
// JSON (vs. mapstructure or a hand-rolled map walker) is load-bearing
// because Profile's time.Time fields and Status enum normalize through
// the standard json package — and any future Profile UnmarshalJSON
// customization will apply automatically.
func decodeProfileArg(req mcp.CallToolRequest, key string) (registry.Profile, *mcp.CallToolResult) {
	raw, ok := req.GetArguments()[key]
	if !ok {
		return registry.Profile{}, toolError("invalid_request", fmt.Sprintf("%s is required", key))
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		return registry.Profile{}, toolError("invalid_request", fmt.Sprintf("%s must be a JSON object", key))
	}
	b, err := json.Marshal(obj)
	if err != nil {
		return registry.Profile{}, toolError("invalid_request", fmt.Sprintf("re-marshal %s: %v", key, err))
	}
	var p registry.Profile
	if err := json.Unmarshal(b, &p); err != nil {
		return registry.Profile{}, toolError("invalid_request", fmt.Sprintf("decode %s: %v", key, err))
	}
	return p, nil
}

// decodePatchArg pulls the object argument named key and decodes it into a
// registry.UpdatePatch. The JSON round-trip is load-bearing for the
// ArrayPatch shorthand-vs-explicit dispatch (registry.ArrayPatch.UnmarshalJSON).
func decodePatchArg(req mcp.CallToolRequest, key string) (registry.UpdatePatch, *mcp.CallToolResult) {
	raw, ok := req.GetArguments()[key]
	if !ok {
		return registry.UpdatePatch{}, toolError("invalid_request", fmt.Sprintf("%s is required", key))
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		return registry.UpdatePatch{}, toolError("invalid_request", fmt.Sprintf("%s must be a JSON object", key))
	}
	b, err := json.Marshal(obj)
	if err != nil {
		return registry.UpdatePatch{}, toolError("invalid_request", fmt.Sprintf("re-marshal %s: %v", key, err))
	}
	var p registry.UpdatePatch
	if err := json.Unmarshal(b, &p); err != nil {
		return registry.UpdatePatch{}, toolError("invalid_request", fmt.Sprintf("decode %s: %v", key, err))
	}
	return p, nil
}

// mapRegistryErr converts a registry service error to an MCP tool-error
// envelope. ErrNoCallback is NOT handled here — Sync's handler treats it
// as a success result. See package comment for the full mapping table.
func mapRegistryErr(err error) *mcp.CallToolResult {
	switch {
	case errors.Is(err, registry.ErrNotFound):
		return toolError("not_found", err.Error())
	case errors.Is(err, registry.ErrInvalidRequest):
		return toolError("invalid_request", err.Error())
	case errors.Is(err, registry.ErrNoResolver):
		return toolError("invalid_request", err.Error())
	case errors.Is(err, registry.ErrPayloadInvalid),
		errors.Is(err, registry.ErrPayloadTooLarge),
		errors.Is(err, registry.ErrPathOutsideRoot):
		return toolError("internal_error", err.Error())
	case errors.Is(err, registry.ErrMintExhausted):
		return toolError("internal_error", err.Error())
	default:
		return toolError("internal_error", err.Error())
	}
}
