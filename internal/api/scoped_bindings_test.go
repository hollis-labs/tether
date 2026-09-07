package api

// scoped_bindings_test.go — end-to-end HTTP coverage for
// /registry/scoped-bindings (T08, messaging vNext). Reuses
// registry_test.go's regServer harness (a real *registry.Service over a
// real in-memory SQLite).

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/hollis-labs/tether/internal/registry"
)

func setScopedBinding(t *testing.T, r *regServer, scope, slot, createdBy string, targets ...string) (*http.Response, registry.ScopedBinding) {
	t.Helper()
	resp, body := r.do(http.MethodPost, "/registry/scoped-bindings", map[string]any{
		"scope": scope, "slot": slot,
		"target_urns": targets,
		"created_by":  createdBy,
	})
	var out registry.ScopedBinding
	if resp.StatusCode == http.StatusCreated {
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("decode: %v (body=%s)", err, body)
		}
	}
	return resp, out
}

func TestScopedBindings_SetThenResolve(t *testing.T) {
	r := newRegServer(t)
	owner, err := r.svc.Register(context.Background(), registry.KindAgent, registry.Profile{DisplayName: "Owner", LastUpdatedBy: "tester"})
	if err != nil {
		t.Fatalf("register owner: %v", err)
	}

	resp, _ := setScopedBinding(t, r, "run-42", "reviewer", owner.URN, "msg://agent/agent-mux/agt_reviewer01")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("set: status=%d", resp.StatusCode)
	}

	getResp, body := r.do(http.MethodGet, "/registry/scoped-bindings/resolve?scope=run-42&slot=reviewer", nil)
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("resolve status = %d, want 200, body=%s", getResp.StatusCode, body)
	}
	var out registry.ScopedBinding
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.TargetURNs) != 1 || out.TargetURNs[0] != "msg://agent/agent-mux/agt_reviewer01" {
		t.Fatalf("resolved = %+v, want one target", out)
	}
}

func TestScopedBindings_ResolveSingle_AmbiguousIsConflict(t *testing.T) {
	r := newRegServer(t)
	owner, err := r.svc.Register(context.Background(), registry.KindAgent, registry.Profile{DisplayName: "Owner", LastUpdatedBy: "tester"})
	if err != nil {
		t.Fatalf("register owner: %v", err)
	}

	resp, _ := setScopedBinding(t, r, "run-42", "engineer", owner.URN, "msg://agent/agent-mux/agt_a", "msg://agent/agent-mux/agt_b")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("set: status=%d", resp.StatusCode)
	}

	getResp, _ := r.do(http.MethodGet, "/registry/scoped-bindings/resolve?scope=run-42&slot=engineer&single=true", nil)
	if getResp.StatusCode != http.StatusConflict {
		t.Fatalf("resolve single (ambiguous): status = %d, want 409", getResp.StatusCode)
	}
}

func TestScopedBindings_Resolve_NotFound(t *testing.T) {
	r := newRegServer(t)
	resp, _ := r.do(http.MethodGet, "/registry/scoped-bindings/resolve?scope=nope&slot=nope", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestScopedBindings_Revisions_TracksHistory(t *testing.T) {
	r := newRegServer(t)
	owner, err := r.svc.Register(context.Background(), registry.KindAgent, registry.Profile{DisplayName: "Owner", LastUpdatedBy: "tester"})
	if err != nil {
		t.Fatalf("register owner: %v", err)
	}
	for _, target := range []string{"msg://agent/agent-mux/agt_v1", "msg://agent/agent-mux/agt_v2"} {
		resp, _ := setScopedBinding(t, r, "run-1", "lead", owner.URN, target)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("set %s: status=%d", target, resp.StatusCode)
		}
	}

	resp, body := r.do(http.MethodGet, "/registry/scoped-bindings/revisions?scope=run-1&slot=lead", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("revisions status = %d, body=%s", resp.StatusCode, body)
	}
	var out struct {
		Revisions []registry.ScopedBinding `json:"revisions"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Revisions) != 2 {
		t.Fatalf("revisions = %+v, want 2", out.Revisions)
	}
}

func TestScopedBindings_Set_RequiresScopeAndSlot(t *testing.T) {
	r := newRegServer(t)
	resp, body := r.do(http.MethodPost, "/registry/scoped-bindings", map[string]any{
		"target_urns": []string{"msg://agent/agent-mux/agt_x"},
		"created_by":  "msg://agent/agent-mux/agt_owner001",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s, want 400", resp.StatusCode, body)
	}
}
