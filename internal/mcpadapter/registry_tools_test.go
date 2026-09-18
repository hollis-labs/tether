package mcpadapter

// registry_tools_test.go — end-to-end coverage for the
// tether_registry_* MCP tools (T-v060-01-06, converted to daemon routing
// in T08). Each test wires a real *registry.Service over a real
// *registry.Storage on an in-memory SQLite DB, stands up a real
// internal/api HTTP test server in front of it, and drives the tools
// through the in-process MCP client with a.client pointed at that server
// (NewWithDaemon) — mirroring internal/mcpadapter/sanitize_integration_test.go's
// newTestAdapterWithDaemon pattern. This also mirrors
// internal/api/registry_test.go's shape one layer down.
//
// Coverage:
//   - tether_registry_register: happy path; caller-supplied URN rejection.
//   - tether_registry_lookup:   happy path; not_found on missing URN.
//   - tether_registry_search:   empty filter; each scalar filter; capability;
//                               status='*' includes deprecated.
//   - tether_registry_update_self: scalar update; array append patch; missing
//                               last_updated_by rejection.
//   - tether_registry_deregister: happy path returns deprecated profile; 404.
//   - tether_registry_sync:     no-callback returns ok+synced=false (not error);
//                               file:// callback round-trip refreshes the row.
//   - nil-guard: a.client == nil (daemon not wired) returns an internal_error
//                envelope, not a panic.

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gomcp "github.com/hollis-labs/go-mcp/server"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/store"
)

// newRegistryAdapter builds an Adapter wired with a live registry.Service
// backed by an in-memory SQLite DB, plus a real internal/api HTTP test
// server (T08: registry tools now route through the daemon, the same
// mux-mcp split-brain fix T05 applied to message tools) and an
// internal/client.Client pointed at it via NewWithDaemon. Optional
// ServiceOptions let individual tests register Sync resolvers. The
// adapter holds ScopeRegistryWrite so mutating tools dispatch; tests that
// need to validate scope-gating can pass empty scopes via
// newRegistryAdapterWithScopes.
func newRegistryAdapter(t *testing.T, opts ...registry.ServiceOption) (*Adapter, *registry.Service) {
	t.Helper()
	return newRegistryAdapterWithScopes(t, []string{ScopeRegistryWrite}, opts...)
}

func newRegistryAdapterWithScopes(t *testing.T, scopes []string, opts ...registry.ServiceOption) (*Adapter, *registry.Service) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := store.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	storage := registry.NewStorage(db)
	svc := registry.NewService(storage, opts...)

	srv := httptest.NewServer(api.NewHandler(api.Deps{Registry: svc}))
	t.Cleanup(srv.Close)
	dc := client.New("tcp:" + strings.TrimPrefix(srv.URL, "http://"))

	a := NewWithDaemon(&app.Service{Registry: svc}, dc, "test-token", scopes)
	return a, svc
}

// callRegistryTool dispatches a tool by name through an in-memory MCP
// client. Args is the tool arguments map (nil for read tools with no args).
// The returned *mcpsdk.CallToolResult mirrors what a real agent would see.
func callRegistryTool(t *testing.T, a *Adapter, name string, args map[string]any) *mcpsdk.CallToolResult {
	t.Helper()
	s := gomcp.NewServer("test", "0.0.1")
	a.registerRegistryTools(s)
	a.registerBindingsTools(s)
	a.registerWhoamiTools(s)
	a.registerScopedBindingsTools(s)

	c := connectInMemory(t, s)
	res, err := c.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool %s: %v", name, err)
	}
	return res
}

// registerAgentForTest creates a minimal agent profile and returns its URN.
// Used to seed the DB for lookup / update / deregister / sync tests.
func registerAgentForTest(t *testing.T, a *Adapter, displayName string) string {
	t.Helper()
	res := callRegistryTool(t, a, "tether_registry_register", map[string]any{
		"kind": "agent",
		"profile": map[string]any{
			"display_name": displayName,
		},
	})
	if res.IsError {
		t.Fatalf("register: %s", textOf(res))
	}
	body := parseToolJSON(t, res)
	profile, ok := body["profile"].(map[string]any)
	if !ok {
		t.Fatalf("register: missing profile in response: %v", body)
	}
	urn, _ := profile["urn"].(string)
	if urn == "" {
		t.Fatalf("register: empty urn in response: %v", profile)
	}
	return urn
}

// ─── register ─────────────────────────────────────────────────────────────────

func TestRegistryTools_Register_Happy(t *testing.T) {
	a, _ := newRegistryAdapter(t)
	res := callRegistryTool(t, a, "tether_registry_register", map[string]any{
		"kind": "agent",
		"profile": map[string]any{
			"display_name": "Auditor",
			"role":         "reviewer",
			"capabilities": []any{"audit", "review"},
		},
	})
	if res.IsError {
		t.Fatalf("register error: %s", textOf(res))
	}
	body := parseToolJSON(t, res)
	if body["ok"] != true {
		t.Errorf("ok = %v, want true", body["ok"])
	}
	profile, _ := body["profile"].(map[string]any)
	if got, _ := profile["display_name"].(string); got != "Auditor" {
		t.Errorf("display_name = %q, want Auditor", got)
	}
	urn, _ := profile["urn"].(string)
	if urn == "" {
		t.Error("urn is empty; server should have minted one")
	}
}

func TestRegistryTools_Register_RejectsCallerURN(t *testing.T) {
	a, _ := newRegistryAdapter(t)
	res := callRegistryTool(t, a, "tether_registry_register", map[string]any{
		"kind": "agent",
		"profile": map[string]any{
			"urn":          "msg://agent/agent-mux/agt_caller_supp",
			"display_name": "Bad",
		},
	})
	if !res.IsError {
		t.Fatalf("expected error result for caller-supplied URN; got %s", textOf(res))
	}
	body := parseToolJSON(t, res)
	if body["code"] != "invalid_request" {
		t.Errorf("code = %v, want invalid_request", body["code"])
	}
}

func TestRegistryTools_Register_RequiresKind(t *testing.T) {
	a, _ := newRegistryAdapter(t)
	res := callRegistryTool(t, a, "tether_registry_register", map[string]any{
		"profile": map[string]any{"display_name": "Anon"},
	})
	if !res.IsError {
		t.Fatalf("expected error for missing kind; got %s", textOf(res))
	}
	if code := parseToolJSON(t, res)["code"]; code != "invalid_request" {
		t.Errorf("code = %v, want invalid_request", code)
	}
}

func TestRegistryTools_Register_RequiresScope(t *testing.T) {
	// Adapter holds no scopes — write tools must reject before dispatch.
	a, _ := newRegistryAdapterWithScopes(t, nil)
	res := callRegistryTool(t, a, "tether_registry_register", map[string]any{
		"kind":    "agent",
		"profile": map[string]any{"display_name": "X"},
	})
	if !res.IsError {
		t.Fatal("expected error result for missing scope")
	}
}

// ─── lookup ───────────────────────────────────────────────────────────────────

func TestRegistryTools_Lookup_Happy(t *testing.T) {
	a, _ := newRegistryAdapter(t)
	urn := registerAgentForTest(t, a, "Looker")

	res := callRegistryTool(t, a, "tether_registry_lookup", map[string]any{"urn": urn})
	if res.IsError {
		t.Fatalf("lookup error: %s", textOf(res))
	}
	body := parseToolJSON(t, res)
	profile, _ := body["profile"].(map[string]any)
	if got, _ := profile["urn"].(string); got != urn {
		t.Errorf("urn = %q, want %q", got, urn)
	}
	if got, _ := profile["display_name"].(string); got != "Looker" {
		t.Errorf("display_name = %q, want Looker", got)
	}
}

func TestRegistryTools_Lookup_NotFound(t *testing.T) {
	a, _ := newRegistryAdapter(t)
	res := callRegistryTool(t, a, "tether_registry_lookup", map[string]any{
		"urn": "msg://agent/agent-mux/agt_doesnotexist",
	})
	if !res.IsError {
		t.Fatal("expected error result for unknown URN")
	}
	if code := parseToolJSON(t, res)["code"]; code != "not_found" {
		t.Errorf("code = %v, want not_found", code)
	}
}

func TestRegistryTools_Lookup_MissingURN(t *testing.T) {
	a, _ := newRegistryAdapter(t)
	res := callRegistryTool(t, a, "tether_registry_lookup", map[string]any{})
	if !res.IsError {
		t.Fatal("expected error for missing urn")
	}
	if code := parseToolJSON(t, res)["code"]; code != "invalid_request" {
		t.Errorf("code = %v, want invalid_request", code)
	}
}

// ─── search ───────────────────────────────────────────────────────────────────

func TestRegistryTools_Search_EmptyAndFiltered(t *testing.T) {
	a, _ := newRegistryAdapter(t)

	// Seed three agents with varied roles + capabilities.
	mustReg := func(payload map[string]any) string {
		t.Helper()
		res := callRegistryTool(t, a, "tether_registry_register", map[string]any{
			"kind": "agent", "profile": payload,
		})
		if res.IsError {
			t.Fatalf("seed register: %s", textOf(res))
		}
		body := parseToolJSON(t, res)
		profile, _ := body["profile"].(map[string]any)
		urn, _ := profile["urn"].(string)
		return urn
	}
	mustReg(map[string]any{
		"display_name": "Alpha",
		"role":         "reviewer",
		"capabilities": []any{"audit"},
	})
	mustReg(map[string]any{
		"display_name": "Bravo",
		"role":         "builder",
		"capabilities": []any{"build", "ship"},
		"project":      "demo",
	})
	mustReg(map[string]any{
		"display_name": "Charlie",
		"role":         "reviewer",
	})

	// Empty filter: returns all three (alphabetical).
	res := callRegistryTool(t, a, "tether_registry_search", map[string]any{"kind": "agent"})
	if res.IsError {
		t.Fatalf("search empty: %s", textOf(res))
	}
	body := parseToolJSON(t, res)
	profiles, _ := body["profiles"].([]any)
	if len(profiles) != 3 {
		t.Errorf("empty filter: got %d profiles, want 3", len(profiles))
	}

	// Filter by role.
	res = callRegistryTool(t, a, "tether_registry_search", map[string]any{
		"kind": "agent", "role": "reviewer",
	})
	if res.IsError {
		t.Fatalf("search role: %s", textOf(res))
	}
	body = parseToolJSON(t, res)
	profiles, _ = body["profiles"].([]any)
	if len(profiles) != 2 {
		t.Errorf("role filter: got %d profiles, want 2", len(profiles))
	}

	// Filter by project.
	res = callRegistryTool(t, a, "tether_registry_search", map[string]any{
		"kind": "agent", "project": "demo",
	})
	if res.IsError {
		t.Fatalf("search project: %s", textOf(res))
	}
	if len(parseToolJSON(t, res)["profiles"].([]any)) != 1 {
		t.Error("project filter: want 1 row")
	}

	// Filter by capability.
	res = callRegistryTool(t, a, "tether_registry_search", map[string]any{
		"kind": "agent", "capability": "audit",
	})
	if res.IsError {
		t.Fatalf("search capability: %s", textOf(res))
	}
	if len(parseToolJSON(t, res)["profiles"].([]any)) != 1 {
		t.Error("capability filter: want 1 row")
	}
}

func TestRegistryTools_Search_StatusWildcardIncludesDeprecated(t *testing.T) {
	a, _ := newRegistryAdapter(t)
	urn := registerAgentForTest(t, a, "ToDeprecate")

	if res := callRegistryTool(t, a, "tether_registry_deregister", map[string]any{"urn": urn}); res.IsError {
		t.Fatalf("deregister: %s", textOf(res))
	}

	// Default search omits deprecated.
	res := callRegistryTool(t, a, "tether_registry_search", map[string]any{"kind": "agent"})
	if res.IsError {
		t.Fatalf("search default: %s", textOf(res))
	}
	if len(parseToolJSON(t, res)["profiles"].([]any)) != 0 {
		t.Error("default search should exclude deprecated rows")
	}

	// status='*' returns all statuses.
	res = callRegistryTool(t, a, "tether_registry_search", map[string]any{
		"kind": "agent", "status": "*",
	})
	if res.IsError {
		t.Fatalf("search status=*: %s", textOf(res))
	}
	if len(parseToolJSON(t, res)["profiles"].([]any)) != 1 {
		t.Error("status=* should include the deprecated row")
	}
}

// ─── update_self ──────────────────────────────────────────────────────────────

func TestRegistryTools_UpdateSelf_ScalarAndArray(t *testing.T) {
	a, _ := newRegistryAdapter(t)
	urn := registerAgentForTest(t, a, "Original")

	res := callRegistryTool(t, a, "tether_registry_update_self", map[string]any{
		"urn": urn,
		"patch": map[string]any{
			"last_updated_by": "test:mcp",
			"display_name":    "Renamed",
			// Array append patch: explicit {mode,value}.
			"capabilities": map[string]any{
				"mode":  "append",
				"value": []any{"audit", "ship"},
			},
		},
	})
	if res.IsError {
		t.Fatalf("update_self: %s", textOf(res))
	}
	body := parseToolJSON(t, res)
	profile, _ := body["profile"].(map[string]any)
	if got, _ := profile["display_name"].(string); got != "Renamed" {
		t.Errorf("display_name = %q, want Renamed", got)
	}
	caps, _ := profile["capabilities"].([]any)
	if len(caps) != 2 {
		t.Errorf("capabilities = %v, want 2 entries", caps)
	}
	if got, _ := profile["last_updated_by"].(string); got != "test:mcp" {
		t.Errorf("last_updated_by = %q, want test:mcp", got)
	}
}

func TestRegistryTools_UpdateSelf_ShorthandArray(t *testing.T) {
	a, _ := newRegistryAdapter(t)
	urn := registerAgentForTest(t, a, "Shorthand")

	// Shorthand `[...]` is parsed as ArrayModeReplace by registry.ArrayPatch.
	res := callRegistryTool(t, a, "tether_registry_update_self", map[string]any{
		"urn": urn,
		"patch": map[string]any{
			"last_updated_by": "test:mcp",
			"capabilities":    []any{"only_this"},
		},
	})
	if res.IsError {
		t.Fatalf("update_self shorthand: %s", textOf(res))
	}
	profile, _ := parseToolJSON(t, res)["profile"].(map[string]any)
	caps, _ := profile["capabilities"].([]any)
	if len(caps) != 1 {
		t.Errorf("shorthand replace: got %d caps, want 1", len(caps))
	}
}

func TestRegistryTools_UpdateSelf_RequiresLastUpdatedBy(t *testing.T) {
	a, _ := newRegistryAdapter(t)
	urn := registerAgentForTest(t, a, "Picky")

	res := callRegistryTool(t, a, "tether_registry_update_self", map[string]any{
		"urn":   urn,
		"patch": map[string]any{"display_name": "Renamed"}, // no last_updated_by
	})
	if !res.IsError {
		t.Fatal("expected error for missing last_updated_by")
	}
	if code := parseToolJSON(t, res)["code"]; code != "invalid_request" {
		t.Errorf("code = %v, want invalid_request", code)
	}
}

// ─── deregister ───────────────────────────────────────────────────────────────

func TestRegistryTools_Deregister_Happy(t *testing.T) {
	a, _ := newRegistryAdapter(t)
	urn := registerAgentForTest(t, a, "Doomed")

	res := callRegistryTool(t, a, "tether_registry_deregister", map[string]any{"urn": urn})
	if res.IsError {
		t.Fatalf("deregister: %s", textOf(res))
	}
	profile, _ := parseToolJSON(t, res)["profile"].(map[string]any)
	if got, _ := profile["status"].(string); got != "deprecated" {
		t.Errorf("status = %q, want deprecated", got)
	}
}

func TestRegistryTools_Deregister_NotFound(t *testing.T) {
	a, _ := newRegistryAdapter(t)
	res := callRegistryTool(t, a, "tether_registry_deregister", map[string]any{
		"urn": "msg://agent/agent-mux/agt_ghost00000",
	})
	if !res.IsError {
		t.Fatal("expected error for unknown URN")
	}
	if code := parseToolJSON(t, res)["code"]; code != "not_found" {
		t.Errorf("code = %v, want not_found", code)
	}
}

// ─── sync ─────────────────────────────────────────────────────────────────────

func TestRegistryTools_Sync_NoCallback(t *testing.T) {
	a, _ := newRegistryAdapter(t)
	urn := registerAgentForTest(t, a, "Lonely") // no callback configured

	res := callRegistryTool(t, a, "tether_registry_sync", map[string]any{"urn": urn})
	if res.IsError {
		t.Fatalf("sync no-callback should NOT be an error: %s", textOf(res))
	}
	body := parseToolJSON(t, res)
	if body["ok"] != true {
		t.Errorf("ok = %v, want true", body["ok"])
	}
	if body["synced"] != false {
		t.Errorf("synced = %v, want false", body["synced"])
	}
}

func TestRegistryTools_Sync_FileCallback(t *testing.T) {
	// Set up a file-callback fixture, register the resolver pointing at the
	// fixture root, then register an agent whose callback points at the file.
	root := t.TempDir()
	fixturePath := filepath.Join(root, "agent.json")
	updatedProfile := map[string]any{
		"display_name": "Refreshed",
		"project":      "synced-project",
		"role":         "synced-role",
		"capabilities": []string{"freshly-synced"},
	}
	payload, err := json.Marshal(updatedProfile)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	if err := os.WriteFile(fixturePath, payload, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	resolver, err := registry.NewFileResolver(root)
	if err != nil {
		t.Fatalf("file resolver: %v", err)
	}
	a, _ := newRegistryAdapter(t, registry.WithResolver(resolver))

	// Register with a file:// callback pointing at the fixture.
	regRes := callRegistryTool(t, a, "tether_registry_register", map[string]any{
		"kind": "agent",
		"profile": map[string]any{
			"display_name": "Original",
			"role":         "original-role",
			"callback": map[string]any{
				"scheme": "file",
				"target": "file://" + fixturePath,
			},
		},
	})
	if regRes.IsError {
		t.Fatalf("register w/ callback: %s", textOf(regRes))
	}
	urn := parseToolJSON(t, regRes)["profile"].(map[string]any)["urn"].(string)

	// Now sync.
	res := callRegistryTool(t, a, "tether_registry_sync", map[string]any{"urn": urn})
	if res.IsError {
		t.Fatalf("sync: %s", textOf(res))
	}
	body := parseToolJSON(t, res)
	if body["synced"] != true {
		t.Errorf("synced = %v, want true", body["synced"])
	}
	profile, _ := body["profile"].(map[string]any)
	if got, _ := profile["display_name"].(string); got != "Refreshed" {
		t.Errorf("display_name = %q, want Refreshed", got)
	}
	if got, _ := profile["project"].(string); got != "synced-project" {
		t.Errorf("project = %q, want synced-project", got)
	}
	// Authored fields must NOT be overwritten by Sync (CW-20260912-0095)
	if got, _ := profile["role"].(string); got != "original-role" {
		t.Errorf("role = %q, want original-role (authored field must not be overwritten)", got)
	}
	caps, _ := profile["capabilities"].([]any)
	if len(caps) != 0 {
		t.Errorf("capabilities = %v, want empty (authored field must not be overwritten)", caps)
	}
	// cached_at should now be populated.
	cachedAt, _ := profile["cached_at"].(string)
	if cachedAt == "" {
		t.Error("cached_at should be set after sync")
	}
	// Sanity-check that cached_at parses as RFC3339.
	if _, err := time.Parse(time.RFC3339Nano, cachedAt); err != nil {
		t.Errorf("cached_at not RFC3339: %v", err)
	}
}

func TestRegistryTools_Sync_NotFound(t *testing.T) {
	a, _ := newRegistryAdapter(t)
	res := callRegistryTool(t, a, "tether_registry_sync", map[string]any{
		"urn": "msg://agent/agent-mux/agt_ghost00000",
	})
	if !res.IsError {
		t.Fatal("expected error for unknown URN")
	}
	if code := parseToolJSON(t, res)["code"]; code != "not_found" {
		t.Errorf("code = %v, want not_found", code)
	}
}

// ─── nil-guard ────────────────────────────────────────────────────────────────

func TestRegistryTools_NilGuard(t *testing.T) {
	// Adapter built via New (in-process only, no daemon client wired) —
	// every registry tool now requires daemon routing (T08) and returns an
	// internal_error envelope rather than panicking when a.client is nil.
	a := New(&app.Service{}, "test-token", []string{ScopeRegistryWrite})

	res := callRegistryTool(t, a, "tether_registry_lookup", map[string]any{
		"urn": "msg://agent/agent-mux/agt_anything00",
	})
	if !res.IsError {
		t.Fatal("nil-guard: expected error result when Registry is nil")
	}
	if code := parseToolJSON(t, res)["code"]; code != "internal_error" {
		t.Errorf("code = %v, want internal_error", code)
	}
}

// ─── C3: Onboarding, Torque substrate, and scope gating ─────────────────────────

func TestRegistryTools_Lookup_ExternalIDsAllowedWithoutWriteScope(t *testing.T) {
	ctx := context.Background()
	// Create adapter with write scope to seed data.
	aWrite, svc := newRegistryAdapter(t)

	// Register project with an external ID and callback.
	proj, err := svc.Register(ctx, registry.KindProject, registry.Profile{
		DisplayName: "Correlated Project",
		Callback: &registry.Callback{
			Scheme: "cli",
			Target: "echo ok",
		},
		ExternalIDs: []registry.ExternalID{
			{Substrate: "torque", ExternalID: "PRJ-0042"},
		},
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Create adapter with NO scopes (read-only).
	srv := httptest.NewServer(api.NewHandler(api.Deps{Registry: svc}))
	t.Cleanup(srv.Close)
	dc := client.New("tcp:" + strings.TrimPrefix(srv.URL, "http://"))
	aRead := NewWithDaemon(&app.Service{Registry: svc}, dc, "test-token", nil)

	// 1. Lookup with include="external_ids" succeeds WITHOUT write scope.
	res := callRegistryTool(t, aRead, "tether_registry_lookup", map[string]any{
		"urn":     proj.URN,
		"include": "external_ids",
	})
	if res.IsError {
		t.Fatalf("expected include=external_ids to succeed without write scope: %s", textOf(res))
	}
	body := parseToolJSON(t, res)
	pMap, _ := body["profile"].(map[string]any)
	exts, _ := pMap["external_ids"].([]any)
	if len(exts) != 1 {
		t.Fatalf("external_ids count = %d, want 1; map = %+v", len(exts), pMap)
	}
	firstExt, _ := exts[0].(map[string]any)
	if got := firstExt["substrate"]; got != "torque" {
		t.Errorf("substrate = %v, want torque", got)
	}
	if got := firstExt["external_id"]; got != "PRJ-0042" {
		t.Errorf("external_id = %v, want PRJ-0042", got)
	}

	// 2. Lookup with include="callback" FAILS without write scope.
	resDenied := callRegistryTool(t, aRead, "tether_registry_lookup", map[string]any{
		"urn":     proj.URN,
		"include": "callback",
	})
	if !resDenied.IsError {
		t.Fatal("expected include=callback to fail without write scope")
	}

	// 3. LookupBy with substrate="torque" and include="external_ids" succeeds without write scope.
	resLookupBy := callRegistryTool(t, aRead, "tether_registry_lookup_by", map[string]any{
		"kind":        "project",
		"external_id": "PRJ-0042",
		"substrate":   "torque",
		"include":     "external_ids",
	})
	if resLookupBy.IsError {
		t.Fatalf("expected lookup-by with include=external_ids to succeed: %s", textOf(resLookupBy))
	}
	bodyLookupBy := parseToolJSON(t, resLookupBy)
	pMapBy, _ := bodyLookupBy["profile"].(map[string]any)
	if got := pMapBy["urn"]; got != proj.URN {
		t.Errorf("lookup-by URN = %v, want %s", got, proj.URN)
	}
	extsBy, _ := pMapBy["external_ids"].([]any)
	if len(extsBy) != 1 {
		t.Fatalf("lookup-by external_ids count = %d, want 1", len(extsBy))
	}

	// 4. LookupBy with include="callback" FAILS without write scope.
	resLookupByDenied := callRegistryTool(t, aRead, "tether_registry_lookup_by", map[string]any{
		"kind":        "project",
		"external_id": "PRJ-0042",
		"substrate":   "torque",
		"include":     "callback",
	})
	if !resLookupByDenied.IsError {
		t.Fatal("expected lookup-by include=callback to fail without write scope")
	}
	_ = aWrite
}

func TestRegistryTools_LookupBy_TorqueSubstrateAndPartialCoverage(t *testing.T) {
	ctx := context.Background()
	a, svc := newRegistryAdapter(t)

	// Seed Project A: tether only (partial coverage: missing torque).
	projA, err := svc.Register(ctx, registry.KindProject, registry.Profile{
		DisplayName: "Project Alpha",
		ExternalIDs: []registry.ExternalID{
			{Substrate: "tether", ExternalID: "alpha"},
		},
	})
	if err != nil {
		t.Fatalf("Register A: %v", err)
	}

	// Seed Project B: tether + torque.
	projB, err := svc.Register(ctx, registry.KindProject, registry.Profile{
		DisplayName: "Project Beta",
		ExternalIDs: []registry.ExternalID{
			{Substrate: "tether", ExternalID: "beta"},
			{Substrate: "torque", ExternalID: "PRJ-BETA"},
		},
	})
	if err != nil {
		t.Fatalf("Register B: %v", err)
	}

	// 1. Reverse lookup on torque resolves Project B.
	resB := callRegistryTool(t, a, "tether_registry_lookup_by", map[string]any{
		"kind":        "project",
		"external_id": "PRJ-BETA",
		"substrate":   "torque",
	})
	if resB.IsError {
		t.Fatalf("lookup-by PRJ-BETA: %s", textOf(resB))
	}
	bodyB := parseToolJSON(t, resB)
	pMapB, _ := bodyB["profile"].(map[string]any)
	if got := pMapB["urn"]; got != projB.URN {
		t.Errorf("URN = %v, want %s", got, projB.URN)
	}

	// 2. Reverse lookup for unattached torque ID on Project A returns not_found cleanly.
	resMissing := callRegistryTool(t, a, "tether_registry_lookup_by", map[string]any{
		"kind":        "project",
		"external_id": "PRJ-ALPHA",
		"substrate":   "torque",
	})
	if !resMissing.IsError {
		t.Fatal("expected not_found for unattached torque external_id")
	}
	if code := parseToolJSON(t, resMissing)["code"]; code != "not_found" {
		t.Errorf("code = %v, want not_found", code)
	}

	// 3. Project A lookup with include="external_ids" returns an ordinary answer
	// omitting torque (never an error or missing-data sentinel).
	resA := callRegistryTool(t, a, "tether_registry_lookup", map[string]any{
		"urn":     projA.URN,
		"include": "external_ids",
	})
	if resA.IsError {
		t.Fatalf("lookup A: %s", textOf(resA))
	}
	bodyA := parseToolJSON(t, resA)
	pMapA, _ := bodyA["profile"].(map[string]any)
	extsA, _ := pMapA["external_ids"].([]any)
	if len(extsA) != 1 {
		t.Fatalf("external_ids count = %d, want 1", len(extsA))
	}
	ext0, _ := extsA[0].(map[string]any)
	if ext0["substrate"] != "tether" {
		t.Errorf("substrate = %v, want tether", ext0["substrate"])
	}
}

func TestRegistryTools_Props(t *testing.T) {
	a, svc := newRegistryAdapter(t)

	// 1. Register with props
	regRes := callRegistryTool(t, a, "tether_registry_register", map[string]any{
		"kind": "project",
		"profile": map[string]any{
			"display_name": "MCP Props Project",
			"description":  "Props via MCP",
			"props": map[string]string{
				"docs_url": "https://mcp.example.com",
				"inbox":    "msg://agent/agent-mux/inbox",
			},
		},
	})
	if regRes.IsError {
		t.Fatalf("register failed: %s", textOf(regRes))
	}
	body := parseToolJSON(t, regRes)
	pMap, _ := body["profile"].(map[string]any)
	urn, _ := pMap["urn"].(string)
	propsMap, _ := pMap["props"].(map[string]any)
	if propsMap["docs_url"] != "https://mcp.example.com" {
		t.Errorf("props[docs_url] = %v; want https://mcp.example.com", propsMap["docs_url"])
	}

	// 2. Default lookup (read scope only) returns props (visible by default)
	srv := httptest.NewServer(api.NewHandler(api.Deps{Registry: svc}))
	t.Cleanup(srv.Close)
	dc := client.New("tcp:" + strings.TrimPrefix(srv.URL, "http://"))
	readAdapter := NewWithDaemon(&app.Service{Registry: svc}, dc, "test-token", nil)

	lookRes := callRegistryTool(t, readAdapter, "tether_registry_lookup", map[string]any{
		"urn": urn,
	})
	if lookRes.IsError {
		t.Fatalf("lookup failed: %s", textOf(lookRes))
	}
	lookBody := parseToolJSON(t, lookRes)
	lookPMap, _ := lookBody["profile"].(map[string]any)
	lookProps, _ := lookPMap["props"].(map[string]any)
	if lookProps["inbox"] != "msg://agent/agent-mux/inbox" {
		t.Errorf("lookProps[inbox] = %v; want msg://agent/agent-mux/inbox", lookProps["inbox"])
	}

	// 3. Update self modifies props
	upRes := callRegistryTool(t, a, "tether_registry_update_self", map[string]any{
		"urn": urn,
		"patch": map[string]any{
			"last_updated_by": "test:operator",
			"props": map[string]string{
				"docs_url": "", // delete
				"repo":     "https://github.com/example/repo",
			},
		},
	})
	if upRes.IsError {
		t.Fatalf("update_self failed: %s", textOf(upRes))
	}
	upBody := parseToolJSON(t, upRes)
	upPMap, _ := upBody["profile"].(map[string]any)
	upProps, _ := upPMap["props"].(map[string]any)
	if _, ok := upProps["docs_url"]; ok {
		t.Errorf("docs_url was not deleted from props")
	}
	if upProps["repo"] != "https://github.com/example/repo" {
		t.Errorf("upProps[repo] = %v; want https://github.com/example/repo", upProps["repo"])
	}
	if upProps["inbox"] != "msg://agent/agent-mux/inbox" {
		t.Errorf("upProps[inbox] = %v; want preserved", upProps["inbox"])
	}
}
