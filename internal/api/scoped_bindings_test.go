package api

// scoped_bindings_test.go — T08 (messaging vNext, CW-20260906-0039): HTTP
// coverage for the scoped role/slot binding surface (a T04 primitive,
// exposed over HTTP for the first time in this task). Reuses
// registry_test.go's newRegServer harness.

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/hollis-labs/tether/internal/registry"
)

func TestScopedBinding_SetThenResolve(t *testing.T) {
	r := newRegServer(t)
	resp, body := r.do(http.MethodPost, "/registry/scoped-bindings", map[string]any{
		"scope": "torque:sprint:X", "slot": "reviewer",
		"target_urns": []string{"msg://agent/agent-mux/agt_reviewer1"},
		"created_by":  "sprint-owner",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("set: status = %d, body = %s", resp.StatusCode, body)
	}
	var created registry.ScopedBinding
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if created.Revision != 1 {
		t.Fatalf("revision = %d, want 1", created.Revision)
	}

	resp, body = r.do(http.MethodGet, "/registry/scoped-bindings?scope=torque:sprint:X&slot=reviewer", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("resolve: status = %d, body = %s", resp.StatusCode, body)
	}
	var resolved registry.ScopedBinding
	if err := json.Unmarshal(body, &resolved); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resolved.TargetURNs) != 1 || resolved.TargetURNs[0] != "msg://agent/agent-mux/agt_reviewer1" {
		t.Fatalf("resolved targets = %v", resolved.TargetURNs)
	}
}

func TestScopedBinding_Resolve_NotFound(t *testing.T) {
	r := newRegServer(t)
	resp, _ := r.do(http.MethodGet, "/registry/scoped-bindings?scope=never&slot=set", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestScopedBinding_ResolveSingle_AmbiguousIsConflict(t *testing.T) {
	r := newRegServer(t)
	resp, body := r.do(http.MethodPost, "/registry/scoped-bindings", map[string]any{
		"scope": "s", "slot": "engineer",
		"target_urns": []string{"msg://agent/agent-mux/agt_a", "msg://agent/agent-mux/agt_b"},
		"created_by":  "owner",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("set: status = %d, body = %s", resp.StatusCode, body)
	}

	resp, _ = r.do(http.MethodGet, "/registry/scoped-bindings?scope=s&slot=engineer&single=true", nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("resolve single (ambiguous): status = %d, want 409", resp.StatusCode)
	}
}

// TestScopedBinding_RebindDoesNotAffectFrozenHistory is the "later
// rebinding must not move a retry to a different actor accidentally"
// architecture requirement, at the HTTP layer: an older revision's
// content must survive a newer SetScopedBinding call untouched.
func TestScopedBinding_RebindDoesNotAffectFrozenHistory(t *testing.T) {
	r := newRegServer(t)
	scope, slot := "s", "reviewer"
	first := func(urn string) {
		resp, body := r.do(http.MethodPost, "/registry/scoped-bindings", map[string]any{
			"scope": scope, "slot": slot, "target_urns": []string{urn}, "created_by": "owner",
		})
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("set %s: status = %d, body = %s", urn, resp.StatusCode, body)
		}
	}
	first("msg://agent/agent-mux/agt_old")
	first("msg://agent/agent-mux/agt_new")

	resp, body := r.do(http.MethodGet, "/registry/scoped-bindings/history?scope="+scope+"&slot="+slot, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("history: status = %d, body = %s", resp.StatusCode, body)
	}
	var out struct {
		Revisions []registry.ScopedBinding `json:"revisions"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Revisions) != 2 {
		t.Fatalf("revisions = %d, want 2", len(out.Revisions))
	}
	if out.Revisions[0].TargetURNs[0] != "msg://agent/agent-mux/agt_new" {
		t.Fatalf("newest revision target = %v, want agt_new first", out.Revisions[0].TargetURNs)
	}
	if out.Revisions[1].TargetURNs[0] != "msg://agent/agent-mux/agt_old" {
		t.Fatalf("older revision target = %v, want agt_old preserved untouched", out.Revisions[1].TargetURNs)
	}
}

func TestScopedBinding_MissingCreatedBy_400(t *testing.T) {
	r := newRegServer(t)
	resp, _ := r.do(http.MethodPost, "/registry/scoped-bindings", map[string]any{
		"scope": "s", "slot": "reviewer", "target_urns": []string{"msg://agent/agent-mux/agt_a"},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (created_by required for provenance)", resp.StatusCode)
	}
}
