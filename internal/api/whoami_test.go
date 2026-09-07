package api

// whoami_test.go — end-to-end coverage for GET /whoami (T08, messaging
// vNext). Wires a real *registry.Service (Registry + Groups) over an
// in-memory SQLite DB, matching registry_test.go's shape.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/store"
)

func newWhoamiServer(t *testing.T) (*httptest.Server, *registry.Service) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := store.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	svc := registry.NewService(registry.NewStorage(db))
	srv := httptest.NewServer(NewHandler(Deps{Registry: svc, Groups: svc}))
	t.Cleanup(srv.Close)
	return srv, svc
}

// registryWithFailingExternalIDs embeds a real *registry.Service (every
// other method promoted through unchanged) and overrides only
// LookupExternalIDsForURN to force a real backend failure -- proving a
// distinct-review-pass fix: this lookup has no "not found" sentinel at
// all (unlike Profile/Binding), so ANY error from it is unexpected and
// must 500, not be silently swallowed into "no external ids."
type registryWithFailingExternalIDs struct {
	*registry.Service
}

func (registryWithFailingExternalIDs) LookupExternalIDsForURN(context.Context, string) ([]registry.ExternalID, error) {
	return nil, errors.New("simulated backend failure")
}

func TestWhoami_ExternalIDLookupError_Returns500NotSilentlySwallowed(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := store.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	svc := registry.NewService(registry.NewStorage(db))
	wrapped := registryWithFailingExternalIDs{svc}
	srv := httptest.NewServer(NewHandler(Deps{Registry: wrapped, Groups: svc}))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/whoami?as=msg://session/agent-mux/sess_x")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (a real backend failure must not be silently swallowed into an empty external_ids list)", resp.StatusCode)
	}
}

func TestWhoami_RequiresAs(t *testing.T) {
	srv, _ := newWhoamiServer(t)
	resp, err := http.Get(srv.URL + "/whoami")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestWhoami_UnregisteredNeverBoundActor_StillReturns200(t *testing.T) {
	// A private-local, standalone session address that never registered
	// or leased a binding -- self-discovery must still succeed (ADR
	// 0045's trust model is about identity assertion, not registration).
	srv, _ := newWhoamiServer(t)
	resp, err := http.Get(srv.URL + "/whoami?as=" + "msg://session/agent-mux/sess_never_registered")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out whoamiResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Profile != nil || out.Binding != nil || len(out.ExternalIDs) != 0 || len(out.Groups) != 0 {
		t.Fatalf("expected every optional field empty for an unregistered/unbound actor, got %+v", out)
	}
}

func TestWhoami_RegisteredActorWithBindingAndGroup_PopulatesEverything(t *testing.T) {
	srv, svc := newWhoamiServer(t)
	ctx := context.Background()

	agent, err := svc.Register(ctx, registry.KindAgent, registry.Profile{DisplayName: "Worker", LastUpdatedBy: "tester"})
	if err != nil {
		t.Fatalf("register agent: %v", err)
	}

	if err := svc.AttachExternalID(ctx, agent.URN, "cerberus", "worker-1"); err != nil {
		t.Fatalf("attach external id: %v", err)
	}

	owner, err := svc.Register(ctx, registry.KindAgent, registry.Profile{DisplayName: "Owner", LastUpdatedBy: "tester"})
	if err != nil {
		t.Fatalf("register owner: %v", err)
	}
	// Register auto-adds the creator (LastUpdatedBy) as group owner.
	group, err := svc.Register(ctx, registry.KindGroup, registry.Profile{DisplayName: "Room", LastUpdatedBy: owner.URN})
	if err != nil {
		t.Fatalf("register group: %v", err)
	}
	if _, err := svc.AddMember(ctx, group.URN, agent.URN, owner.URN, registry.MemberRoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}

	binding, err := svc.LeaseBinding(ctx, agent.URN, "sess-1", "local", "sess-1", nil, registry.VisibilityPrivateLocal, 0)
	if err != nil {
		t.Fatalf("lease binding: %v", err)
	}

	resp, err := http.Get(srv.URL + "/whoami?as=" + agent.URN)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out whoamiResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Profile == nil || out.Profile.URN != agent.URN {
		t.Fatalf("profile = %+v, want the registered agent", out.Profile)
	}
	if len(out.ExternalIDs) != 1 || out.ExternalIDs[0].ExternalID != "worker-1" {
		t.Fatalf("external_ids = %+v, want [worker-1]", out.ExternalIDs)
	}
	if len(out.Groups) != 1 || out.Groups[0].URN != group.URN {
		t.Fatalf("groups = %+v, want [%s]", out.Groups, group.URN)
	}
	if out.Binding == nil || out.Binding.ID != binding.ID {
		t.Fatalf("binding = %+v, want %+v", out.Binding, binding)
	}
}
