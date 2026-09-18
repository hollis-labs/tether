// Package mcpadapter — registry_tools.go wires the native
// `tether_registry_*` MCP tools for the v0.6 federation directory service
// (T-v060-01-06, extended in T08 messaging vNext).
//
// T08 (messaging vNext, CW-20260906-0039): every handler here now routes
// through a.client (the daemon HTTP client), not a.svc.Registry
// in-process. `mux mcp` opens its OWN separate SQLite connection to
// ~/.tether/state/tether.db, distinct from the running daemon's
// in-process Registry instance (internal/mcpadapter/adapter.go's package
// doc) -- a `mux mcp` registry write previously landed on a DIFFERENT
// connection than the live daemon's, invisible to it until the next
// catalog reload, the exact split-brain bug class T05 closed for message
// tools. The original comment here cited "ADR 0034's 'MCP shares the same
// Service composition root' rule" -- ADR 0034 (docs/adr/0034-acp-surface.md)
// is the ACP surface adoption record and contains no such rule; ADR 0035
// (mcpadapter-daemon-client-routing) is the actual routing decision record,
// and its "Messages stays in-process" carve-out is what T05 later overrode
// for messages. This is the same override applied to registry/group tools.
//
// Tool-prefix choice. Names use the `tether_` prefix (not `mux_`) per D14
// and the sprint spec — the directory service is the first surface where
// "Tether" is the user-facing name. Internal Go packages still say `mux`
// in places; the prefix divergence is intentional.
//
// Scope. Read tools (`lookup`, `search`) are unauthenticated — same-host
// UDS trust per D7 — and reuse the no-scope pattern from skills.go.
// Write tools (`register`, `update_self`, `deregister`, `sync`, `merge`)
// require the new `registry.write` scope. Sync is classified as a write
// because it mutates the cached_at column and replaces thin-profile
// fields from the callback payload.
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
	"strings"

	gomcp "github.com/hollis-labs/go-mcp/server"

	"github.com/hollis-labs/tether/internal/registry"
)

// ScopeRegistryWrite gates the mutating registry tools (register, update_self,
// deregister, sync). New in T-v060-01-06; deliberately separate from
// ScopeCatalogWrite so an operator can grant directory-service writes without
// granting catalog YAML writes, and vice versa.
const ScopeRegistryWrite = "registry.write"

// registerRegistryTools wires the tether_registry_* native tools onto s.
// Every tool routes through a.client to the daemon's /registry HTTP
// surface (T08). Read tools (lookup, lookup_by, search) are
// unauthenticated; write tools (register, update_self, deregister, sync,
// merge) require the registry.write scope.
func (a *Adapter) registerRegistryTools(s *gomcp.Server) {
	a.addTool(s, gomcp.Tool{
		Name: "tether_registry_register",
		Description: "Register a new agent or project profile in the federation directory. " +
			"Server mints the URN (Stripe-style opaque id, e.g. agt_xxxxxxxxxx / prj_xxxxxxxxxx); " +
			"callers MUST NOT supply the urn field — doing so returns invalid_request. " +
			"The kind argument selects the entity kind. The profile argument is a JSON object " +
			"matching registry.Profile: required fields are display_name; optional fields " +
			"include title, role, description, avatar, project, status, callback, capabilities, " +
			"skills (each {name, learned_at RFC3339, optional via, level}), links (each " +
			"{kind, target}), kind_meta, host_address, health_status. " +
			"Returns the canonical Profile with the minted URN. Requires the registry.write scope.",
		InputSchema: gomcp.ObjectSchema(map[string]any{
			"kind":    strEnumProp("Entity kind: 'agent' or 'project'.", "agent", "project"),
			"profile": objProp("Profile JSON to register. See tool description for the field shape."),
		}, "kind", "profile"),
		Handler: a.handleRegistryRegister,
	}, Writes())

	a.addTool(s, gomcp.Tool{
		Name: "tether_registry_lookup",
		Description: "Look up a registry profile by URN. Returns the redacted Profile by default or not_found. " +
			"Soft-deleted (status='deprecated') rows are returned by direct lookup — they " +
			"are excluded only from default Search results. Read-only by default; passing " +
			"sensitive operational fields ('callback', 'kind_meta', 'host_address', 'all') in include " +
			"requires the registry.write scope, while correlation identifiers ('external_ids') are accessible with read scope.",
		InputSchema: gomcp.ObjectSchema(map[string]any{
			"urn":     strProp("Full URN as minted by Register, e.g. msg://agent/agent-mux/agt_xxxxxxxxxx."),
			"include": strProp("Optional comma-separated fields to include (e.g. 'external_ids', 'callback', 'kind_meta', 'host_address') or 'all'. Sensitive operational fields require registry.write scope."),
		}, "urn"),
		Handler: a.handleRegistryLookup,
	}, Reads("registry profile lookup"))

	a.addTool(s, gomcp.Tool{
		Name: "tether_registry_lookup_by",
		Description: "Resolve a substrate-local external ID to one registry profile. Returns 0 or 1 row; " +
			"use this when you know a local ID like a Tether catalog slug, Torque project ID, or Cerberus owner and " +
			"want the canonical registry URN. Read-only; passing sensitive operational fields in include " +
			"requires registry.write scope, while correlation identifiers ('external_ids') are accessible with read scope.",
		InputSchema: gomcp.ObjectSchema(map[string]any{
			"kind":        strEnumProp("Entity kind to resolve: 'agent', 'project', or 'group'.", "agent", "project", "group"),
			"external_id": strProp("Substrate-local identifier to resolve."),
			"substrate":   strProp("Optional substrate scope such as 'tether', 'torque', or 'cerberus'."),
			"include":     strProp("Optional comma-separated fields to include (e.g. 'external_ids'). Sensitive operational fields require registry.write scope."),
		}, "kind", "external_id"),
		Handler: a.handleRegistryLookupBy,
	}, Reads("registry lookup by external id"))

	a.addTool(s, gomcp.Tool{
		Name: "tether_registry_search",
		Description: "Search the registry by filter. All filters combine with AND. Result ordering is " +
			"alphabetical on display_name. Default excludes status='deprecated'; pass " +
			"status='deprecated' to return only deprecated rows, or status='*' to return " +
			"all statuses. Read-only; no scope required.",
		InputSchema: gomcp.ObjectSchema(map[string]any{
			"kind":       strEnumProp("Entity kind to search: 'agent' or 'project'.", "agent", "project"),
			"role":       strProp("Filter on role (exact match)."),
			"title":      strProp("Filter on title (exact match)."),
			"project":    strProp("Filter on project (exact match)."),
			"capability": strProp("Filter to rows that carry this capability string."),
			"skill_name": strProp("Filter to rows that carry a skill with this name."),
			"status":     strProp("Filter on status. Empty → active only; 'deprecated' → deprecated only; '*' → all."),
			"tag":        strProp("Filter to rows that carry this tag string in tags."),
		}, "kind"),
		Handler: a.handleRegistrySearch,
	}, Reads("registry search"))

	a.addTool(s, gomcp.Tool{
		Name: "tether_registry_update_self",
		Description: "Partial-merge update of a registry row. Returns the refreshed Profile. " +
			"Scalar fields (display_name, title, role, description, avatar, project, status, " +
			"health_status, host_address, last_seen_at, kind_meta, guidelines) update column-wise — only " +
			"fields present in the patch are touched. " +
			"Array fields (capabilities, skills, links, tags, entry_points) accept TWO wire shapes:\n" +
			"  (1) Shorthand `[...]` — equivalent to {mode:'replace', value:[...]}.\n" +
			"  (2) Explicit `{mode: 'replace'|'append'|'remove', value: [...]}`.\n" +
			"Empty value is always a no-op (existing arrays are not cleared). " +
			"Remove matches: capabilities, tags, and entry_points by string equality; skills by name; links by (kind, target) tuple. " +
			"last_updated_by is required on every patch — caller-supplied identity string, becomes auth-bound in v060-02. " +
			"Requires the registry.write scope.",
		InputSchema: gomcp.ObjectSchema(map[string]any{
			"urn":   strProp("Full URN of the row to update."),
			"patch": objProp("UpdatePatch JSON. See tool description for partial-merge semantics."),
		}, "urn", "patch"),
		Handler: a.handleRegistryUpdateSelf,
	}, Writes())

	a.addTool(s, gomcp.Tool{
		Name: "tether_registry_deregister",
		Description: "Soft-delete a registry row. Status flips to 'deprecated'. The row remains visible " +
			"via direct URN lookup (so callers can audit deprecated entries); default Search " +
			"excludes it. Returns the deprecated Profile. Requires the registry.write scope.",
		InputSchema: gomcp.ObjectSchema(map[string]any{
			"urn": strProp("Full URN of the row to soft-delete."),
		}, "urn"),
		Handler: a.handleRegistryDeregister,
	}, Destroys("removes the identity and its bindings; anything addressing it stops resolving"))

	a.addTool(s, gomcp.Tool{
		Name: "tether_registry_merge",
		Description: "Merge a source profile into a destination profile: the source's external-ID mappings " +
			"are reattached to the destination and the source row is soft-deleted (status='deprecated'). " +
			"Returns the canonical destination Profile. Requires the registry.write scope.",
		InputSchema: gomcp.ObjectSchema(map[string]any{
			"urn":  strProp("Source URN to merge away."),
			"into": strProp("Destination URN the source's identity mappings are reattached to."),
		}, "urn", "into"),
		Handler: a.handleRegistryMerge,
	}, Writes())

	a.addTool(s, gomcp.Tool{
		Name: "tether_registry_sync",
		Description: "Refresh thin-profile columns from the row's callback URI. Returns one of two success shapes:\n" +
			"  - {ok:true, synced:false} when the row has no callback configured (no-op state).\n" +
			"  - {ok:true, synced:true, profile:<refreshed>} when the callback was invoked successfully.\n" +
			"Raw payload is NEVER stored (substrate ops-store files often contain plaintext secrets); " +
			"only the thin-profile columns + capabilities/skills/links arrays + cached_at are updated. " +
			"Requires the registry.write scope.",
		InputSchema: gomcp.ObjectSchema(map[string]any{
			"urn": strProp("Full URN of the row to sync."),
		}, "urn"),
		Handler: a.handleRegistrySync,
	}, Writes())
}

// ─── handlers ─────────────────────────────────────────────────────────────────

// handleRegistryRegister services tether_registry_register. The profile
// argument arrives as a JSON object; we re-marshal it through json.Marshal
// → json.Unmarshal so registry.Profile's UnmarshalJSON applies (this is
// load-bearing for the ArrayPatch shorthand handling on capabilities/skills/
// links — though Profile uses plain []T, not ArrayPatch[T]). The map → JSON
// → struct round-trip also normalizes the time.Time fields.
func (a *Adapter) handleRegistryRegister(ctx context.Context, args map[string]any) (any, error) {
	if err := a.checkScope(ScopeRegistryWrite); err != nil {
		return nil, err
	}

	kind, err := requireKind(args)
	if err != nil {
		return nil, err
	}

	profile, err := decodeProfileArg(args, "profile")
	if err != nil {
		return nil, err
	}

	if a.client == nil {
		return nil, toolError("internal_error", "tether_registry_register requires daemon routing; start MCP with mux mcp")
	}
	out, err := a.client.Registry().Register(ctx, kind, profile)
	if err != nil {
		if isDaemonUnreachable(err) {
			return nil, daemonUnreachableError(err)
		}
		return nil, mapRegistryErr(err)
	}
	return toolJSON(map[string]any{"ok": true, "profile": out}), nil
}

// requiresRegistryWriteScope reports whether the given include parameter
// requests sensitive operational fields (callback, host_address, kind_meta,
// or all/*). Requests for external_ids alone are cross-system correlation
// identifiers and do not require elevated write scope.
func requiresRegistryWriteScope(include string) bool {
	if include == "" {
		return false
	}
	parts := strings.Split(include, ",")
	for _, part := range parts {
		trimmed := strings.TrimSpace(strings.ToLower(part))
		switch trimmed {
		case "external_ids", "externalids", "":
			continue
		default:
			return true
		}
	}
	return false
}

// handleRegistryLookup services tether_registry_lookup. Read-only by default;
// passing sensitive operational fields in include requires the registry.write scope.
func (a *Adapter) handleRegistryLookup(ctx context.Context, args map[string]any) (any, error) {
	urn := str(args, "urn")
	if urn == "" {
		return nil, toolError("invalid_request", "urn is required")
	}
	if a.client == nil {
		return nil, toolError("internal_error", "tether_registry_lookup requires daemon routing; start MCP with mux mcp")
	}
	include := str(args, "include")
	if requiresRegistryWriteScope(include) {
		if err := a.checkScope(ScopeRegistryWrite); err != nil {
			return nil, err
		}
	}
	var (
		out registry.Profile
		err error
	)
	if include != "" {
		out, err = a.client.Registry().LookupWithInclude(ctx, urn, include)
	} else {
		out, err = a.client.Registry().Lookup(ctx, urn)
	}
	if err != nil {
		if isDaemonUnreachable(err) {
			return nil, daemonUnreachableError(err)
		}
		return nil, mapRegistryErr(err)
	}
	return toolJSON(map[string]any{"ok": true, "profile": out}), nil
}

func (a *Adapter) handleRegistryLookupBy(ctx context.Context, args map[string]any) (any, error) {
	kind, err := requireKind(args)
	if err != nil {
		return nil, err
	}
	externalID := str(args, "external_id")
	if externalID == "" {
		return nil, toolError("invalid_request", "external_id is required")
	}
	if a.client == nil {
		return nil, toolError("internal_error", "tether_registry_lookup_by requires daemon routing; start MCP with mux mcp")
	}
	include := str(args, "include")
	if requiresRegistryWriteScope(include) {
		if err := a.checkScope(ScopeRegistryWrite); err != nil {
			return nil, err
		}
	}
	var out registry.Profile
	if include != "" {
		out, err = a.client.Registry().LookupByWithInclude(ctx, kind, externalID, str(args, "substrate"), include)
	} else {
		out, err = a.client.Registry().LookupBy(ctx, kind, externalID, str(args, "substrate"))
	}
	if err != nil {
		if isDaemonUnreachable(err) {
			return nil, daemonUnreachableError(err)
		}
		return nil, mapRegistryErr(err)
	}
	return toolJSON(map[string]any{"ok": true, "profile": out}), nil
}

// handleRegistrySearch services tether_registry_search. Read-only; no scope.
func (a *Adapter) handleRegistrySearch(ctx context.Context, args map[string]any) (any, error) {
	kind, err := requireKind(args)
	if err != nil {
		return nil, err
	}
	f := registry.Filter{
		Role:       str(args, "role"),
		Title:      str(args, "title"),
		Project:    str(args, "project"),
		Capability: str(args, "capability"),
		SkillName:  str(args, "skill_name"),
		Status:     str(args, "status"),
		Tag:        str(args, "tag"),
	}
	if a.client == nil {
		return nil, toolError("internal_error", "tether_registry_search requires daemon routing; start MCP with mux mcp")
	}
	out, err := a.client.Registry().Search(ctx, kind, f)
	if err != nil {
		if isDaemonUnreachable(err) {
			return nil, daemonUnreachableError(err)
		}
		return nil, mapRegistryErr(err)
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
func (a *Adapter) handleRegistryUpdateSelf(ctx context.Context, args map[string]any) (any, error) {
	if err := a.checkScope(ScopeRegistryWrite); err != nil {
		return nil, err
	}
	urn := str(args, "urn")
	if urn == "" {
		return nil, toolError("invalid_request", "urn is required")
	}
	patch, err := decodePatchArg(args, "patch")
	if err != nil {
		return nil, err
	}
	if a.client == nil {
		return nil, toolError("internal_error", "tether_registry_update_self requires daemon routing; start MCP with mux mcp")
	}
	out, err := a.client.Registry().UpdateSelf(ctx, urn, patch)
	if err != nil {
		if isDaemonUnreachable(err) {
			return nil, daemonUnreachableError(err)
		}
		return nil, mapRegistryErr(err)
	}
	return toolJSON(map[string]any{"ok": true, "profile": out}), nil
}

// handleRegistryDeregister services tether_registry_deregister.
func (a *Adapter) handleRegistryDeregister(ctx context.Context, args map[string]any) (any, error) {
	if err := a.checkScope(ScopeRegistryWrite); err != nil {
		return nil, err
	}
	urn := str(args, "urn")
	if urn == "" {
		return nil, toolError("invalid_request", "urn is required")
	}
	if a.client == nil {
		return nil, toolError("internal_error", "tether_registry_deregister requires daemon routing; start MCP with mux mcp")
	}
	out, err := a.client.Registry().Deregister(ctx, urn)
	if err != nil {
		if isDaemonUnreachable(err) {
			return nil, daemonUnreachableError(err)
		}
		return nil, mapRegistryErr(err)
	}
	return toolJSON(map[string]any{"ok": true, "profile": out}), nil
}

// handleRegistryMerge services tether_registry_merge.
func (a *Adapter) handleRegistryMerge(ctx context.Context, args map[string]any) (any, error) {
	if err := a.checkScope(ScopeRegistryWrite); err != nil {
		return nil, err
	}
	urn := str(args, "urn")
	into := str(args, "into")
	if urn == "" || into == "" {
		return nil, toolError("invalid_request", "urn and into are required")
	}
	if a.client == nil {
		return nil, toolError("internal_error", "tether_registry_merge requires daemon routing; start MCP with mux mcp")
	}
	out, err := a.client.Registry().Merge(ctx, urn, into)
	if err != nil {
		if isDaemonUnreachable(err) {
			return nil, daemonUnreachableError(err)
		}
		return nil, mapRegistryErr(err)
	}
	return toolJSON(map[string]any{"ok": true, "profile": out}), nil
}

// handleRegistrySync services tether_registry_sync. Distinct from the rest
// of the dispatch matrix because "no callback configured" is a success
// result (see Service.Sync's godoc), not an error envelope --
// RegistryClient.Sync surfaces that as its own synced=false return value
// (HTTP 204) rather than a registry.ErrNoCallback sentinel, since the
// sentinel doesn't cross the wire.
func (a *Adapter) handleRegistrySync(ctx context.Context, args map[string]any) (any, error) {
	if err := a.checkScope(ScopeRegistryWrite); err != nil {
		return nil, err
	}
	urn := str(args, "urn")
	if urn == "" {
		return nil, toolError("invalid_request", "urn is required")
	}
	if a.client == nil {
		return nil, toolError("internal_error", "tether_registry_sync requires daemon routing; start MCP with mux mcp")
	}
	out, synced, err := a.client.Registry().Sync(ctx, urn)
	if err != nil {
		if isDaemonUnreachable(err) {
			return nil, daemonUnreachableError(err)
		}
		return nil, mapRegistryErr(err)
	}
	if !synced {
		return toolJSON(map[string]any{"ok": true, "synced": false}), nil
	}
	return toolJSON(map[string]any{"ok": true, "synced": true, "profile": out}), nil
}

// ─── helpers ─────────────────────────────────────────────────────────────────

// requireKind reads the kind argument and validates it against the v060-01
// vocabulary (agent + project). Service.Register/Search also validates,
// but checking here lets the error message name the argument explicitly.
func requireKind(args map[string]any) (registry.Kind, error) {
	raw := str(args, "kind")
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
func decodeProfileArg(args map[string]any, key string) (registry.Profile, error) {
	raw, ok := args[key]
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
func decodePatchArg(args map[string]any, key string) (registry.UpdatePatch, error) {
	raw, ok := args[key]
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
func mapRegistryErr(err error) error {
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
	case errors.Is(err, registry.ErrStaleGeneration), errors.Is(err, registry.ErrVisibilityConflict):
		// T08: binding-lease/renew conflicts (a newer generation exists, or
		// the target is already bound to a Tether-managed session) are
		// real, expected outcomes of concurrent-actor-session handling and
		// the T07 supersede guard -- not server errors.
		return toolError("conflict", err.Error())
	default:
		return toolError("internal_error", err.Error())
	}
}
