package mcpadapter

// scoped_bindings_tools_test.go — end-to-end coverage for the
// tether_registry_scoped_binding_* MCP tools (T08). Reuses
// registry_tools_test.go's daemon-routed harness.

import "testing"

func TestScopedBindingTools_SetResolveRevisions_Roundtrip(t *testing.T) {
	a, _ := newRegistryAdapter(t)

	setRes := callRegistryTool(t, a, "tether_registry_scoped_binding_set", map[string]any{
		"scope": "run-1", "slot": "reviewer",
		"target_urns": []any{"msg://agent/agent-mux/agt_a"},
		"created_by":  "msg://agent/agent-mux/agt_owner",
	})
	if setRes.IsError {
		t.Fatalf("set: %v", setRes.Content)
	}
	setBody := parseToolJSON(t, setRes)
	binding, _ := setBody["binding"].(map[string]any)
	if binding["Revision"] != float64(1) {
		t.Fatalf("revision = %v, want 1", binding["Revision"])
	}

	resolveRes := callRegistryTool(t, a, "tether_registry_scoped_binding_resolve", map[string]any{
		"scope": "run-1", "slot": "reviewer",
	})
	if resolveRes.IsError {
		t.Fatalf("resolve: %v", resolveRes.Content)
	}

	singleRes := callRegistryTool(t, a, "tether_registry_scoped_binding_resolve", map[string]any{
		"scope": "run-1", "slot": "reviewer", "single": true,
	})
	if singleRes.IsError {
		t.Fatalf("resolve single: %v", singleRes.Content)
	}
	singleBody := parseToolJSON(t, singleRes)
	if singleBody["target_urn"] != "msg://agent/agent-mux/agt_a" {
		t.Fatalf("target_urn = %v, want agt_a", singleBody["target_urn"])
	}

	// Second revision makes resolve-single ambiguous.
	if res := callRegistryTool(t, a, "tether_registry_scoped_binding_set", map[string]any{
		"scope": "run-1", "slot": "reviewer",
		"target_urns": []any{"msg://agent/agent-mux/agt_a", "msg://agent/agent-mux/agt_b"},
		"created_by":  "msg://agent/agent-mux/agt_owner",
	}); res.IsError {
		t.Fatalf("set 2: %v", res.Content)
	}
	ambiguousRes := callRegistryTool(t, a, "tether_registry_scoped_binding_resolve", map[string]any{
		"scope": "run-1", "slot": "reviewer", "single": true,
	})
	if !ambiguousRes.IsError {
		t.Fatal("expected an error resolving a now-ambiguous binding")
	}
	if code := parseToolJSON(t, ambiguousRes)["code"]; code != "conflict" {
		t.Errorf("code = %v, want conflict", code)
	}

	revisionsRes := callRegistryTool(t, a, "tether_registry_scoped_binding_revisions", map[string]any{
		"scope": "run-1", "slot": "reviewer",
	})
	if revisionsRes.IsError {
		t.Fatalf("revisions: %v", revisionsRes.Content)
	}
	revisionsBody := parseToolJSON(t, revisionsRes)
	revisions, _ := revisionsBody["revisions"].([]any)
	if len(revisions) != 2 {
		t.Fatalf("revisions = %+v, want 2", revisionsBody)
	}
}

func TestScopedBindingTools_Set_RequiresScope(t *testing.T) {
	a, _ := newRegistryAdapterWithScopes(t, nil)
	res := callRegistryTool(t, a, "tether_registry_scoped_binding_set", map[string]any{
		"scope": "run-1", "slot": "reviewer",
		"target_urns": []any{"msg://agent/agent-mux/agt_a"},
		"created_by":  "msg://agent/agent-mux/agt_owner",
	})
	if !res.IsError {
		t.Fatal("expected an error without registry.write scope")
	}
	if code := parseToolJSON(t, res)["code"]; code != "insufficient_scope" {
		t.Errorf("code = %v, want insufficient_scope", code)
	}
}

func TestScopedBindingTools_Resolve_NotFound(t *testing.T) {
	a, _ := newRegistryAdapter(t)
	res := callRegistryTool(t, a, "tether_registry_scoped_binding_resolve", map[string]any{
		"scope": "nope", "slot": "nope",
	})
	if !res.IsError {
		t.Fatal("expected an error for an unset scope/slot")
	}
	if code := parseToolJSON(t, res)["code"]; code != "not_found" {
		t.Errorf("code = %v, want not_found", code)
	}
}
