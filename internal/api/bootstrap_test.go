package api

// bootstrap_test.go — T08 (messaging vNext, CW-20260906-0039): coverage
// for POST /bootstrap/session against the sprint's own required fixture
// list: standalone session-only, durable actor, missing provider ID,
// repeated hook calls, explicit publication, private offline boot, and
// reconnect.

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/store"
)

func newBootstrapServer(t *testing.T) (*httptest.Server, *registry.Service) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := store.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	reg := registry.NewService(registry.NewStorage(db))
	srv := httptest.NewServer(NewHandler(Deps{Registry: reg}))
	t.Cleanup(srv.Close)
	return srv, reg
}

// bootstrap POSTs /bootstrap/session, reusing message_claims_test.go's
// postJSON helper (same package).
func bootstrap(t *testing.T, base string, body map[string]any) (int, bootstrapSessionResponse) {
	t.Helper()
	resp, respBody := postJSON(t, base+"/bootstrap/session", body)
	var out bootstrapSessionResponse
	if resp.StatusCode == http.StatusCreated {
		if err := json.Unmarshal(respBody, &out); err != nil {
			t.Fatalf("decode: %v (body=%s)", err, respBody)
		}
	}
	return resp.StatusCode, out
}

// TestBootstrap_StandaloneSessionOnly is the "standalone session-only"
// fixture: no logical_agent_id means no durable actor is claimed -- no
// registry write, no binding, just a session-scoped self-discovery echo.
func TestBootstrap_StandaloneSessionOnly(t *testing.T) {
	srv, _ := newBootstrapServer(t)
	status, out := bootstrap(t, srv.URL, map[string]any{"session_id": "sess-1"})
	if status != http.StatusCreated {
		t.Fatalf("status = %d", status)
	}
	if out.URN != "msg://session/agent-mux/sess-1" {
		t.Fatalf("urn = %q", out.URN)
	}
	if out.Profile != nil || out.Binding != nil || out.RegisteredNow {
		t.Fatalf("standalone boot must not register or bind anything, got %+v", out)
	}
}

// TestBootstrap_MissingSessionID_400 is required: a preassigned SESSION
// is the one non-optional input.
func TestBootstrap_MissingSessionID_400(t *testing.T) {
	srv, _ := newBootstrapServer(t)
	resp, body := postJSON(t, srv.URL+"/bootstrap/session", map[string]any{"logical_agent_id": "worker"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s, want 400", resp.StatusCode, body)
	}
}

// TestBootstrap_PrivateOfflineBoot is the "private offline boot" fixture:
// a durable actor named WITHOUT explicit publication gets identity by
// naming only -- no registry Profile, no binding. Matches "a one-off
// session need not manufacture a permanent actor record."
func TestBootstrap_PrivateOfflineBoot(t *testing.T) {
	srv, reg := newBootstrapServer(t)
	status, out := bootstrap(t, srv.URL, map[string]any{
		"session_id": "sess-1", "logical_agent_id": "worker",
	})
	if status != http.StatusCreated {
		t.Fatalf("status = %d", status)
	}
	if out.Profile != nil || out.Binding != nil || out.RegisteredNow {
		t.Fatalf("private boot must not register or bind, got %+v", out)
	}
	if _, err := reg.CurrentBinding(context.Background(), registry.LogicalAgentBindingTarget("worker")); err == nil {
		t.Fatalf("expected no binding to exist after a private-local bootstrap")
	}
}

// TestBootstrap_DurableActor_ExplicitPublication is the "durable actor" +
// "explicit publication" fixtures combined: publication=published-local
// registers (idempotently) and leases a real, resolvable binding.
func TestBootstrap_DurableActor_ExplicitPublication(t *testing.T) {
	srv, reg := newBootstrapServer(t)
	status, out := bootstrap(t, srv.URL, map[string]any{
		"session_id": "sess-1", "logical_agent_id": "worker", "publication": "published-local",
	})
	if status != http.StatusCreated {
		t.Fatalf("status = %d", status)
	}
	if !out.RegisteredNow {
		t.Fatalf("expected a fresh registration on the first published-local bootstrap")
	}
	if out.Binding == nil || out.Binding.SessionID != "sess-1" || out.Binding.Visibility != string(registry.VisibilityPublishedLocal) {
		t.Fatalf("binding = %+v, want SessionID=sess-1 Visibility=published-local", out.Binding)
	}
	if out.Profile == nil {
		t.Fatalf("expected a registered profile in the response")
	}

	current, err := reg.CurrentBinding(context.Background(), registry.LogicalAgentBindingTarget("worker"))
	if err != nil || current.SessionID != "sess-1" {
		t.Fatalf("current binding = %+v, err=%v", current, err)
	}
}

// TestBootstrap_RepeatedHookCalls_Idempotent is the "repeated hook calls"
// fixture: the SAME session_id/host_id/attempt_id must not mint a
// redundant generation on a second call.
func TestBootstrap_RepeatedHookCalls_Idempotent(t *testing.T) {
	srv, reg := newBootstrapServer(t)
	req := map[string]any{
		"session_id": "sess-1", "logical_agent_id": "worker", "publication": "published-local",
		"host_id": "h1", "attempt_id": "a1",
	}
	status, first := bootstrap(t, srv.URL, req)
	if status != http.StatusCreated || first.ReusedBinding {
		t.Fatalf("first call: status=%d reused=%v, want 201 not-reused", status, first.ReusedBinding)
	}
	firstGen := first.Binding.Generation

	status, second := bootstrap(t, srv.URL, req)
	if status != http.StatusCreated {
		t.Fatalf("second call: status = %d", status)
	}
	if !second.ReusedBinding {
		t.Fatalf("second identical call: expected ReusedBinding=true, got %+v", second)
	}
	if second.RegisteredNow {
		t.Fatalf("second call: expected RegisteredNow=false (already registered), got true")
	}
	if second.Binding.Generation != firstGen {
		t.Fatalf("second identical call minted a new generation (%d -> %d), want unchanged", firstGen, second.Binding.Generation)
	}

	bindings, err := reg.ListBindingsForTarget(context.Background(), registry.LogicalAgentBindingTarget("worker"))
	if err != nil {
		t.Fatalf("list bindings: %v", err)
	}
	if len(bindings) != 1 {
		t.Fatalf("bindings leased = %d, want exactly 1 (no wasted generation on repeat)", len(bindings))
	}
}

// TestBootstrap_Reconnect_NewAttempt_MintsNewGeneration is the
// "reconnect" fixture: a genuinely different attempt_id (the process
// actually restarted) is a legitimate reconnect, not a no-op -- it mints
// a new, fenced generation as normal.
func TestBootstrap_Reconnect_NewAttempt_MintsNewGeneration(t *testing.T) {
	srv, _ := newBootstrapServer(t)
	status, first := bootstrap(t, srv.URL, map[string]any{
		"session_id": "sess-1", "logical_agent_id": "worker", "publication": "published-local",
		"host_id": "h1", "attempt_id": "a1",
	})
	if status != http.StatusCreated {
		t.Fatalf("first: status = %d", status)
	}

	status, second := bootstrap(t, srv.URL, map[string]any{
		"session_id": "sess-1", "logical_agent_id": "worker", "publication": "published-local",
		"host_id": "h1", "attempt_id": "a2", // reconnect: new attempt
	})
	if status != http.StatusCreated {
		t.Fatalf("reconnect: status = %d", status)
	}
	if second.ReusedBinding {
		t.Fatalf("a genuinely new attempt_id must mint a new generation, not report reused")
	}
	if second.Binding.Generation <= first.Binding.Generation {
		t.Fatalf("reconnect generation = %d, want greater than first %d", second.Binding.Generation, first.Binding.Generation)
	}
}

// TestBootstrap_MissingProviderID_NoCrash is the "missing provider ID"
// fixture: provider_id is optional and simply unused by this endpoint.
func TestBootstrap_MissingProviderID_NoCrash(t *testing.T) {
	srv, _ := newBootstrapServer(t)
	status, _ := bootstrap(t, srv.URL, map[string]any{
		"session_id": "sess-1", "logical_agent_id": "worker", "publication": "published-local",
	})
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want 201 even with no provider_id", status)
	}
}

// TestBootstrap_CannotSupersedeManagedSession reuses the same guard
// bindings.go's own lease endpoint uses (refuseToSupersedeManagedSession)
// -- bootstrap cannot silently take over a Tether-managed session either.
func TestBootstrap_CannotSupersedeManagedSession(t *testing.T) {
	srv, reg := newBootstrapServer(t)
	target := registry.LogicalAgentBindingTarget("worker")
	if _, err := reg.LeaseBinding(context.Background(), target, "real-session", "local", "real-session", nil, registry.VisibilityPrivateLocal, 0); err != nil {
		t.Fatalf("seed managed binding: %v", err)
	}

	resp, body := postJSON(t, srv.URL+"/bootstrap/session", map[string]any{
		"session_id": "bridge-1", "logical_agent_id": "worker", "publication": "published-local",
	})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, body = %s, want 409", resp.StatusCode, body)
	}
}
