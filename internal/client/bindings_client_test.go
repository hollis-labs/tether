package client

// bindings_client_test.go — end-to-end coverage for the typed
// BindingsClient (T08). Unlike registry_client_test.go's hand-rolled
// http.HandlerFunc fixtures, these stand up a REAL internal/api.Server
// over a REAL *registry.Service (real SQLite, real generation fencing) --
// the bindings surface's behavior (supersede guard, generation bumps,
// expiry) lives in registry.Storage.leaseBinding, not in wire-format
// glue, so a real backing service is the more meaningful fixture here.

import (
	"context"
	"database/sql"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/store"
)

func newBindingsTestClient(t *testing.T) (*BindingsClient, *registry.Service) {
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

	srv := httptest.NewServer(api.NewHandler(api.Deps{Registry: svc}))
	t.Cleanup(srv.Close)
	c := &Client{baseURL: srv.URL, http: srv.Client()}
	return c.Bindings(), svc
}

func TestBindingsClient_LeaseCurrentListRenewRevoke_Roundtrip(t *testing.T) {
	ctx := context.Background()
	bc, _ := newBindingsTestClient(t)
	target := "msg://agent/agent-mux/worker"

	leased, err := bc.Lease(ctx, target, "bridge-1", "host-1", "attempt-1", []string{"pull-only"}, 0)
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	if leased.Visibility != registry.VisibilityPublishedLocal {
		t.Fatalf("visibility = %q, want published-local", leased.Visibility)
	}
	if leased.Generation != 1 {
		t.Fatalf("generation = %d, want 1", leased.Generation)
	}

	cur, err := bc.Current(ctx, target)
	if err != nil {
		t.Fatalf("current: %v", err)
	}
	if cur.ID != leased.ID {
		t.Fatalf("current = %+v, want the just-leased binding", cur)
	}

	renewed, err := bc.Renew(ctx, leased.ID, 3600)
	if err != nil {
		t.Fatalf("renew: %v", err)
	}
	if renewed.LeaseExpiresAt == nil {
		t.Fatalf("renewed lease has no expiry, want one set from ttl_seconds=3600")
	}

	if err := bc.Revoke(ctx, leased.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := bc.Current(ctx, target); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("current after revoke: got %v, want ErrNotFound", err)
	}

	// Revoke is idempotent.
	if err := bc.Revoke(ctx, leased.ID); err != nil {
		t.Fatalf("revoke (repeat): %v", err)
	}

	all, err := bc.ListForTarget(ctx, target)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 1 || all[0].ID != leased.ID {
		t.Fatalf("list = %+v, want the one (revoked-but-still-listed) binding", all)
	}
}

func TestBindingsClient_Lease_CannotSupersedeTetherManagedBinding(t *testing.T) {
	ctx := context.Background()
	bc, svc := newBindingsTestClient(t)
	target := "msg://agent/agent-mux/worker"

	if _, err := svc.LeaseBinding(ctx, target, "real-session", "local", "real-session", nil, registry.VisibilityPrivateLocal, 0); err != nil {
		t.Fatalf("seed private-local binding: %v", err)
	}

	_, err := bc.Lease(ctx, target, "bridge-1", "host-1", "attempt-1", []string{"pull-only"}, 0)
	if !errors.Is(err, registry.ErrVisibilityConflict) {
		t.Fatalf("lease over a Tether-managed binding: got %v, want ErrVisibilityConflict", err)
	}
}

func TestBindingsClient_NotFound(t *testing.T) {
	ctx := context.Background()
	bc, _ := newBindingsTestClient(t)

	// A distinct review pass found readBindingError's 404 case only
	// wrapped registry.ErrNotFound, not the daemon's actual source
	// sentinel registry.ErrBindingNotFound (bindings.go) -- a latent trap
	// for any future caller checking the more specific sentinel. Both
	// must match.
	if _, err := bc.Current(ctx, "msg://agent/agent-mux/never-bound"); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("current for a never-bound target: got %v, want ErrNotFound", err)
	}
	if _, err := bc.Current(ctx, "msg://agent/agent-mux/never-bound"); !errors.Is(err, registry.ErrBindingNotFound) {
		t.Fatalf("current for a never-bound target: got %v, want ErrBindingNotFound", err)
	}
	if err := bc.Revoke(ctx, "no-such-binding-id"); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("revoke unknown binding id: got %v, want ErrNotFound", err)
	}
	if err := bc.Revoke(ctx, "no-such-binding-id"); !errors.Is(err, registry.ErrBindingNotFound) {
		t.Fatalf("revoke unknown binding id: got %v, want ErrBindingNotFound", err)
	}
}

func TestBindingsClient_Unreachable(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(nil)
	c := &Client{baseURL: srv.URL, http: srv.Client()}
	srv.Close()

	_, err := c.Bindings().Current(ctx, "msg://agent/agent-mux/worker")
	if !errors.Is(err, ErrDaemonUnreachable) {
		t.Fatalf("current after server close: got %v, want ErrDaemonUnreachable", err)
	}
}

func TestBindingsClient_ListForTarget_EmptyNotNil(t *testing.T) {
	ctx := context.Background()
	bc, _ := newBindingsTestClient(t)
	out, err := bc.ListForTarget(ctx, "msg://agent/agent-mux/never-bound")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if out == nil {
		t.Fatalf("list for a never-bound target returned nil, want a non-nil empty slice")
	}
	if len(out) != 0 {
		t.Fatalf("list = %+v, want empty", out)
	}
}
