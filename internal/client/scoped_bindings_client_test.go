package client

// scoped_bindings_client_test.go — end-to-end coverage for
// ScopedBindingsClient (T08). Stands up a real internal/api.Server over a
// real *registry.Service, matching bindings_client_test.go's shape.

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

func newScopedBindingsTestClient(t *testing.T) *ScopedBindingsClient {
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
	return c.ScopedBindings()
}

func TestScopedBindingsClient_SetResolveRevisions_Roundtrip(t *testing.T) {
	ctx := context.Background()
	sc := newScopedBindingsTestClient(t)

	first, err := sc.Set(ctx, "run-1", "reviewer", []string{"msg://agent/agent-mux/agt_a"}, nil, "msg://agent/agent-mux/agt_owner")
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if first.Revision != 1 {
		t.Fatalf("revision = %d, want 1", first.Revision)
	}

	resolved, err := sc.Resolve(ctx, "run-1", "reviewer")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(resolved.TargetURNs) != 1 || resolved.TargetURNs[0] != "msg://agent/agent-mux/agt_a" {
		t.Fatalf("resolved = %+v", resolved)
	}

	target, _, err := sc.ResolveSingle(ctx, "run-1", "reviewer")
	if err != nil {
		t.Fatalf("resolve single: %v", err)
	}
	if target != "msg://agent/agent-mux/agt_a" {
		t.Fatalf("single target = %q, want agt_a", target)
	}

	second, err := sc.Set(ctx, "run-1", "reviewer", []string{"msg://agent/agent-mux/agt_a", "msg://agent/agent-mux/agt_b"}, nil, "msg://agent/agent-mux/agt_owner")
	if err != nil {
		t.Fatalf("set 2: %v", err)
	}
	if second.Revision != 2 {
		t.Fatalf("revision = %d, want 2", second.Revision)
	}

	if _, _, err := sc.ResolveSingle(ctx, "run-1", "reviewer"); !errors.Is(err, registry.ErrAmbiguousBinding) {
		t.Fatalf("resolve single (now ambiguous): got %v, want ErrAmbiguousBinding", err)
	}

	revisions, err := sc.ListRevisions(ctx, "run-1", "reviewer")
	if err != nil {
		t.Fatalf("list revisions: %v", err)
	}
	if len(revisions) != 2 {
		t.Fatalf("revisions = %+v, want 2", revisions)
	}
}

func TestScopedBindingsClient_Resolve_NotFound(t *testing.T) {
	sc := newScopedBindingsTestClient(t)
	if _, err := sc.Resolve(context.Background(), "nope", "nope"); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}

func TestScopedBindingsClient_Unreachable(t *testing.T) {
	srv := httptest.NewServer(nil)
	c := &Client{baseURL: srv.URL, http: srv.Client()}
	srv.Close()

	if _, err := c.ScopedBindings().Resolve(context.Background(), "s", "s"); !errors.Is(err, ErrDaemonUnreachable) {
		t.Fatalf("got %v, want ErrDaemonUnreachable", err)
	}
}
