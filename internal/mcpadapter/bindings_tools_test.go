package mcpadapter

// bindings_tools_test.go — end-to-end coverage for the
// tether_registry_binding_* MCP tools (T08). Reuses
// registry_tools_test.go's newRegistryAdapterWithScopes harness (a real
// *registry.Service behind a real internal/api HTTP test server, driven
// through a.client via NewWithDaemon).

import (
	"context"
	"testing"
)

func TestBindingTools_LeaseCurrentListRenewRevoke_Roundtrip(t *testing.T) {
	a, _ := newRegistryAdapter(t)
	target := "msg://agent/agent-mux/worker"

	leaseRes := callRegistryTool(t, a, "tether_registry_binding_lease", map[string]any{
		"target_urn": target, "session_id": "bridge-1", "host_id": "host-1", "attempt_id": "attempt-1",
		"capabilities": []any{"pull-only"},
	})
	if leaseRes.IsError {
		t.Fatalf("lease: %v", leaseRes.Content)
	}
	body := parseToolJSON(t, leaseRes)
	binding, _ := body["binding"].(map[string]any)
	bindingID, _ := binding["ID"].(string)
	if bindingID == "" {
		t.Fatalf("lease response missing binding.ID: %+v", body)
	}

	curRes := callRegistryTool(t, a, "tether_registry_binding_current", map[string]any{"target_urn": target})
	if curRes.IsError {
		t.Fatalf("current: %v", curRes.Content)
	}
	curBody := parseToolJSON(t, curRes)
	curBinding, _ := curBody["binding"].(map[string]any)
	if curBinding["ID"] != bindingID {
		t.Fatalf("current binding id = %v, want %v", curBinding["ID"], bindingID)
	}

	renewRes := callRegistryTool(t, a, "tether_registry_binding_renew", map[string]any{
		"binding_id": bindingID, "ttl_seconds": 3600,
	})
	if renewRes.IsError {
		t.Fatalf("renew: %v", renewRes.Content)
	}

	revokeRes := callRegistryTool(t, a, "tether_registry_binding_revoke", map[string]any{"binding_id": bindingID})
	if revokeRes.IsError {
		t.Fatalf("revoke: %v", revokeRes.Content)
	}

	listRes := callRegistryTool(t, a, "tether_registry_binding_list", map[string]any{"target_urn": target})
	if listRes.IsError {
		t.Fatalf("list: %v", listRes.Content)
	}
	listBody := parseToolJSON(t, listRes)
	bindings, _ := listBody["bindings"].([]any)
	if len(bindings) != 1 {
		t.Fatalf("list = %+v, want 1 (revoked-but-still-listed) binding", listBody)
	}
}

func TestBindingTools_Lease_RequiresScope(t *testing.T) {
	a, _ := newRegistryAdapterWithScopes(t, nil)
	res := callRegistryTool(t, a, "tether_registry_binding_lease", map[string]any{
		"target_urn": "msg://agent/agent-mux/worker", "session_id": "s", "host_id": "h", "attempt_id": "a",
		"capabilities": []any{"pull-only"},
	})
	if !res.IsError {
		t.Fatal("expected an error result without registry.write scope")
	}
	if code := parseToolJSON(t, res)["code"]; code != "insufficient_scope" {
		t.Errorf("code = %v, want insufficient_scope", code)
	}
}

func TestBindingTools_Lease_CannotSupersedeTetherManagedBinding(t *testing.T) {
	a, svc := newRegistryAdapter(t)
	target := "msg://agent/agent-mux/worker"
	if _, err := svc.LeaseBinding(context.Background(), target, "real-session", "local", "real-session", nil, "private-local", 0); err != nil {
		t.Fatalf("seed private-local binding: %v", err)
	}

	res := callRegistryTool(t, a, "tether_registry_binding_lease", map[string]any{
		"target_urn": target, "session_id": "bridge-1", "host_id": "host-1", "attempt_id": "attempt-1",
		"capabilities": []any{"pull-only"},
	})
	if !res.IsError {
		t.Fatal("expected an error result superseding a Tether-managed binding")
	}
	if code := parseToolJSON(t, res)["code"]; code != "conflict" {
		t.Errorf("code = %v, want conflict", code)
	}
}

func TestBindingTools_Current_NotFound(t *testing.T) {
	a, _ := newRegistryAdapter(t)
	res := callRegistryTool(t, a, "tether_registry_binding_current", map[string]any{"target_urn": "msg://agent/agent-mux/never-bound"})
	if !res.IsError {
		t.Fatal("expected an error result for a never-bound target")
	}
	if code := parseToolJSON(t, res)["code"]; code != "not_found" {
		t.Errorf("code = %v, want not_found", code)
	}
}
