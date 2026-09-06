package api

// bindings_test.go — T07 (messaging vNext, CW-20260906-0038): HTTP
// coverage for the published-local bridge registration surface
// (/registry/bindings...). Reuses registry_test.go's newRegServer harness
// (a real *registry.Service over a real in-memory SQLite).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hollis-labs/tether/internal/registry"
)

func TestBindingLease_HappyPath_MintsPublishedLocalBinding(t *testing.T) {
	r := newRegServer(t)
	resp, body := r.do(http.MethodPost, "/registry/bindings", map[string]any{
		"target_urn":   "msg://agent/agent-mux/worker",
		"session_id":   "bridge-session-1",
		"host_id":      "external-bridge-host",
		"attempt_id":   "attempt-1",
		"capabilities": []string{"pull-only"},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, body = %s, want 201", resp.StatusCode, body)
	}
	var b registry.RuntimeBinding
	if err := json.Unmarshal(body, &b); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if b.Visibility != registry.VisibilityPublishedLocal {
		t.Fatalf("visibility = %q, want %q -- this endpoint must always mint published-local, never accept a caller-declared visibility", b.Visibility, registry.VisibilityPublishedLocal)
	}
	if len(b.Capabilities) != 1 || b.Capabilities[0] != "pull-only" {
		t.Fatalf("capabilities = %v, want [pull-only]", b.Capabilities)
	}
	if b.SessionID != "bridge-session-1" || b.HostID != "external-bridge-host" {
		t.Fatalf("binding = %+v, want SessionID=bridge-session-1 HostID=external-bridge-host", b)
	}
}

func TestBindingLease_RejectsUnsupportedCapabilities(t *testing.T) {
	r := newRegServer(t)
	cases := [][]string{nil, {}, {"push"}, {"pull-only", "push"}}
	for _, caps := range cases {
		resp, body := r.do(http.MethodPost, "/registry/bindings", map[string]any{
			"target_urn": "msg://agent/agent-mux/worker",
			"session_id": "s1", "host_id": "h1", "attempt_id": "a1",
			"capabilities": caps,
		})
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("capabilities=%v: status = %d, body = %s, want 400 -- push-notified bridging is not implemented and must be declined, not silently accepted", caps, resp.StatusCode, body)
		}
	}
}

func TestBindingLease_RequiresIdentityFields(t *testing.T) {
	r := newRegServer(t)
	resp, _ := r.do(http.MethodPost, "/registry/bindings", map[string]any{
		"target_urn":   "msg://agent/agent-mux/worker",
		"capabilities": []string{"pull-only"},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (missing session_id/host_id/attempt_id)", resp.StatusCode)
	}
}

// TestBindingLease_CannotClaimTetherHostedVisibility documents that the
// request body has no visibility field at all -- there is no way for a
// caller to request anything other than published-local through this
// endpoint. Passing one is simply ignored (extra JSON fields decode into
// nothing, per bindingLeaseRequest's fixed shape).
func TestBindingLease_CannotClaimTetherHostedVisibility(t *testing.T) {
	r := newRegServer(t)
	resp, body := r.do(http.MethodPost, "/registry/bindings", map[string]any{
		"target_urn": "msg://agent/agent-mux/worker",
		"session_id": "s1", "host_id": "h1", "attempt_id": "a1",
		"capabilities": []string{"pull-only"},
		"visibility":   "tether-hosted",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, body = %s, want 201", resp.StatusCode, body)
	}
	var b registry.RuntimeBinding
	if err := json.Unmarshal(body, &b); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if b.Visibility != registry.VisibilityPublishedLocal {
		t.Fatalf("visibility = %q, want %q even though the caller asked for tether-hosted -- the field is not read from the request at all", b.Visibility, registry.VisibilityPublishedLocal)
	}
}

// TestBindingLease_CannotSupersedeTetherManagedSession is a distinct
// review pass finding, fixed here: LeaseBinding itself performs no
// ownership check, so without this guard any same-host caller could
// silence wake delivery for a currently running, healthy local session
// with one call. This endpoint may only supersede an EXISTING
// published-local binding (or bind a never-bound target); taking over a
// private-local (Tether-managed) binding must be refused, observably.
func TestBindingLease_CannotSupersedeTetherManagedSession(t *testing.T) {
	r := newRegServer(t)
	target := "msg://agent/agent-mux/worker"
	// Simulate a real Tether-managed session's binding (the shape
	// internal/app/session_lifecycle.go's leaseActorBinding mints).
	if _, err := r.svc.LeaseBinding(context.Background(), target, "real-session-1", "local", "real-session-1", nil, registry.VisibilityPrivateLocal, 0); err != nil {
		t.Fatalf("seed private-local binding: %v", err)
	}

	resp, body := r.do(http.MethodPost, "/registry/bindings", map[string]any{
		"target_urn": target, "session_id": "bridge-1", "host_id": "external-bridge", "attempt_id": "a1",
		"capabilities": []string{"pull-only"},
	})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("lease over a Tether-managed session: status = %d, body = %s, want 409", resp.StatusCode, body)
	}

	current, err := r.svc.CurrentBinding(context.Background(), target)
	if err != nil {
		t.Fatalf("current binding: %v", err)
	}
	if current.SessionID != "real-session-1" {
		t.Fatalf("current binding session = %q, want the real session to remain current (untouched)", current.SessionID)
	}
}

// TestBindingLease_CanSupersedeExistingPublishedLocalBinding is the
// legitimate bridge-restart case: a new lease over an EXISTING
// published-local binding must still succeed (a bridge process
// restarting and re-registering is normal, not a takeover of a
// Tether-managed session).
func TestBindingLease_CanSupersedeExistingPublishedLocalBinding(t *testing.T) {
	r := newRegServer(t)
	target := "msg://agent/agent-mux/worker"
	resp, body := r.do(http.MethodPost, "/registry/bindings", map[string]any{
		"target_urn": target, "session_id": "bridge-1", "host_id": "h", "attempt_id": "a1",
		"capabilities": []string{"pull-only"},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("first lease: status = %d, body = %s", resp.StatusCode, body)
	}

	resp, body = r.do(http.MethodPost, "/registry/bindings", map[string]any{
		"target_urn": target, "session_id": "bridge-1-restarted", "host_id": "h", "attempt_id": "a2",
		"capabilities": []string{"pull-only"},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("re-lease over an existing published-local binding: status = %d, body = %s, want 201", resp.StatusCode, body)
	}
}

func TestBindingCurrentAndList(t *testing.T) {
	r := newRegServer(t)
	target := "msg://agent/agent-mux/worker"
	lease := func(sessionID string) registry.RuntimeBinding {
		resp, body := r.do(http.MethodPost, "/registry/bindings", map[string]any{
			"target_urn": target, "session_id": sessionID, "host_id": "h", "attempt_id": sessionID,
			"capabilities": []string{"pull-only"},
		})
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("lease %s: status = %d, body = %s", sessionID, resp.StatusCode, body)
		}
		var b registry.RuntimeBinding
		_ = json.Unmarshal(body, &b)
		return b
	}
	lease("s1")
	second := lease("s2")

	resp, body := r.do(http.MethodGet, "/registry/bindings?target_urn="+target+"&current=true", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("current: status = %d, body = %s", resp.StatusCode, body)
	}
	var current registry.RuntimeBinding
	if err := json.Unmarshal(body, &current); err != nil {
		t.Fatalf("decode current: %v", err)
	}
	if current.SessionID != "s2" || current.ID != second.ID {
		t.Fatalf("current = %+v, want the second (newer generation) lease", current)
	}

	resp, body = r.do(http.MethodGet, "/registry/bindings?target_urn="+target, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list: status = %d, body = %s", resp.StatusCode, body)
	}
	var out struct {
		Bindings []registry.RuntimeBinding `json:"bindings"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(out.Bindings) != 2 {
		t.Fatalf("list returned %d bindings, want 2 (full audit history)", len(out.Bindings))
	}
}

func TestBindingCurrent_NotFound(t *testing.T) {
	r := newRegServer(t)
	resp, _ := r.do(http.MethodGet, "/registry/bindings?target_urn=msg://agent/agent-mux/never-bound&current=true", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestBindingRenew(t *testing.T) {
	r := newRegServer(t)
	resp, body := r.do(http.MethodPost, "/registry/bindings", map[string]any{
		"target_urn": "msg://agent/agent-mux/worker", "session_id": "s1", "host_id": "h1", "attempt_id": "a1",
		"capabilities": []string{"pull-only"},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("lease: status = %d, body = %s", resp.StatusCode, body)
	}
	var b registry.RuntimeBinding
	_ = json.Unmarshal(body, &b)

	resp, body = r.do(http.MethodPost, "/registry/bindings/"+b.ID+"/renew", map[string]any{"ttl_seconds": 60})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("renew: status = %d, body = %s", resp.StatusCode, body)
	}
	var renewed registry.RuntimeBinding
	if err := json.Unmarshal(body, &renewed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if renewed.LeaseExpiresAt == nil {
		t.Fatalf("renewed binding has no lease_expires_at, want one set from ttl_seconds")
	}
}

// TestBindingRenew_StaleGenerationIsConflict is the "concurrent actor
// sessions" case at the HTTP layer: renewing a superseded binding must
// fail observably (409), not silently succeed or 500.
func TestBindingRenew_StaleGenerationIsConflict(t *testing.T) {
	r := newRegServer(t)
	target := "msg://agent/agent-mux/worker"
	resp, body := r.do(http.MethodPost, "/registry/bindings", map[string]any{
		"target_urn": target, "session_id": "s1", "host_id": "h1", "attempt_id": "a1",
		"capabilities": []string{"pull-only"},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("first lease: status = %d, body = %s", resp.StatusCode, body)
	}
	var first registry.RuntimeBinding
	_ = json.Unmarshal(body, &first)

	// A second lease supersedes the first.
	if resp, _ := r.do(http.MethodPost, "/registry/bindings", map[string]any{
		"target_urn": target, "session_id": "s2", "host_id": "h1", "attempt_id": "a2",
		"capabilities": []string{"pull-only"},
	}); resp.StatusCode != http.StatusCreated {
		t.Fatalf("second lease failed: %d", resp.StatusCode)
	}

	resp, body = r.do(http.MethodPost, "/registry/bindings/"+first.ID+"/renew", map[string]any{"ttl_seconds": 60})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("renew stale binding: status = %d, body = %s, want 409", resp.StatusCode, body)
	}
}

func TestBindingRevoke(t *testing.T) {
	r := newRegServer(t)
	target := "msg://agent/agent-mux/worker"
	resp, body := r.do(http.MethodPost, "/registry/bindings", map[string]any{
		"target_urn": target, "session_id": "s1", "host_id": "h1", "attempt_id": "a1",
		"capabilities": []string{"pull-only"},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("lease: status = %d, body = %s", resp.StatusCode, body)
	}
	var b registry.RuntimeBinding
	_ = json.Unmarshal(body, &b)

	resp, _ = r.do(http.MethodPost, "/registry/bindings/"+b.ID+"/revoke", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke: status = %d, want 204", resp.StatusCode)
	}

	resp, _ = r.do(http.MethodGet, "/registry/bindings?target_urn="+target+"&current=true", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("current after revoke: status = %d, want 404", resp.StatusCode)
	}
}

func TestBindingRevoke_UnknownID(t *testing.T) {
	r := newRegServer(t)
	resp, _ := r.do(http.MethodPost, "/registry/bindings/does-not-exist/revoke", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestBindingsRoutes_NotConfigured_404(t *testing.T) {
	srv := httptest.NewServer(NewHandler(Deps{})) // no Registry
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL + "/registry/bindings?target_urn=x")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 when Registry is nil", resp.StatusCode)
	}
}
