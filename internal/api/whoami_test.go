package api

// whoami_test.go — T08 (messaging vNext, CW-20260906-0039): coverage for
// GET /whoami, the architecture's never-before-built self-discovery
// endpoint.

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

func newWhoAmIServer(t *testing.T, svc *fakeLaunchService) (*httptest.Server, *registry.Service) {
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
	var launch LaunchService
	if svc != nil {
		launch = svc
	}
	srv := httptest.NewServer(NewHandler(Deps{Registry: reg, Service: launch}))
	t.Cleanup(srv.Close)
	return srv, reg
}

func whoAmI(t *testing.T, base, urn string) (int, whoAmIResponse) {
	t.Helper()
	resp, err := http.Get(base + "/whoami?urn=" + urn)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	var out whoAmIResponse
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	return resp.StatusCode, out
}

func TestWhoAmI_BareURN_NoFurtherState(t *testing.T) {
	srv, _ := newWhoAmIServer(t, nil)
	status, out := whoAmI(t, srv.URL, "msg://agent/agent-mux/worker")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if out.Profile != nil || out.Binding != nil || out.Session != nil || len(out.Groups) != 0 {
		t.Fatalf("expected an otherwise-empty response for a bare, unregistered URN, got %+v", out)
	}
}

func TestWhoAmI_MissingURN_400(t *testing.T) {
	srv, _ := newWhoAmIServer(t, nil)
	resp, err := http.Get(srv.URL + "/whoami")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestWhoAmI_InvalidURN_400(t *testing.T) {
	srv, _ := newWhoAmIServer(t, nil)
	resp, err := http.Get(srv.URL + "/whoami?urn=not-a-urn")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestWhoAmI_ReportsProfileAndBinding(t *testing.T) {
	srv, reg := newWhoAmIServer(t, nil)
	ctx := context.Background()
	target := "msg://agent/agent-mux/worker"

	if _, err := reg.LeaseBinding(ctx, target, "s1", "external-bridge", "a1", []string{"pull-only"}, registry.VisibilityPublishedLocal, 0); err != nil {
		t.Fatalf("lease: %v", err)
	}

	status, out := whoAmI(t, srv.URL, target)
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if out.Binding == nil || out.Binding.SessionID != "s1" || out.Binding.Visibility != string(registry.VisibilityPublishedLocal) {
		t.Fatalf("binding = %+v, want SessionID=s1 Visibility=published-local", out.Binding)
	}
}

func TestWhoAmI_ReportsGroupMemberships(t *testing.T) {
	srv, reg := newWhoAmIServer(t, nil)
	ctx := context.Background()

	member, err := reg.Register(ctx, registry.KindAgent, registry.Profile{DisplayName: "Member", LastUpdatedBy: "tester"})
	if err != nil {
		t.Fatalf("register member: %v", err)
	}
	owner, err := reg.Register(ctx, registry.KindAgent, registry.Profile{DisplayName: "Owner", LastUpdatedBy: "tester"})
	if err != nil {
		t.Fatalf("register owner: %v", err)
	}
	group, err := reg.Register(ctx, registry.KindGroup, registry.Profile{DisplayName: "Room", LastUpdatedBy: owner.URN})
	if err != nil {
		t.Fatalf("register group: %v", err)
	}
	if _, err := reg.AddMember(ctx, group.URN, member.URN, owner.URN, registry.MemberRoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}

	status, out := whoAmI(t, srv.URL, member.URN)
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if len(out.Groups) != 1 || out.Groups[0].URN != group.URN {
		t.Fatalf("groups = %+v, want exactly [%s]", out.Groups, group.URN)
	}
}

// TestWhoAmI_ReportsLiveLocalSession_ForAgentKindURN proves whoami
// surfaces whatever Service.ResolveActorSession resolves for a
// msg://agent/... URN (the exact resolution T06/T07's own tests already
// cover in depth for the real binding-aware implementation -- this test
// only checks whoami wires that result through correctly, using
// fakeLaunchService's own legacy-scan-replicating fields since the fake
// has no concept of registry bindings at all).
func TestWhoAmI_ReportsLiveLocalSession_ForAgentKindURN(t *testing.T) {
	fake := &fakeLaunchService{
		listRes:         []store.SessionRow{{ID: "s1", LogicalAgentID: "worker", State: "running"}},
		runtimeHealthOK: map[string]bool{"s1": true},
	}
	srv, _ := newWhoAmIServer(t, fake)

	status, out := whoAmI(t, srv.URL, "msg://agent/agent-mux/worker")
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if out.Session == nil || out.Session.SessionID != "s1" || out.Session.Host != "local" {
		t.Fatalf("session = %+v, want SessionID=s1 Host=local", out.Session)
	}
}

func TestWhoAmI_ExactSessionURN_ReportsSessionDirectly(t *testing.T) {
	fake := &fakeLaunchService{runtimeHealthOK: map[string]bool{"s1": true}}
	srv, _ := newWhoAmIServer(t, fake)

	status, out := whoAmI(t, srv.URL, "msg://session/agent-mux/s1")
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if out.Session == nil || out.Session.SessionID != "s1" {
		t.Fatalf("session = %+v, want SessionID=s1", out.Session)
	}
}
