package app

// binding_lifecycle_test.go — T06 (messaging vNext, CW-20260906-0037):
// leaseActorBinding/revokeActorBindingIfCurrent, the launch/stop-time
// glue that makes ResolveActorSession's binding-first resolution
// (wake.go) actually authoritative in practice rather than dead T02
// infrastructure. See LaunchSession/StopSession in session_lifecycle.go.

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/store"
)

func TestLeaseActorBinding_NoRegistry_NoOp(t *testing.T) {
	svc := &Service{}
	// Must not panic with a nil Registry -- this is the composition
	// context api.LaunchService's doc comment already calls out ("nil in
	// lighter composition contexts").
	svc.leaseActorBinding("s1", "worker")
}

func TestLeaseActorBinding_EmptyLogicalAgentID_NoOp(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "lease.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	reg := registry.NewService(registry.NewStorage(st.DB()))
	svc := &Service{Store: st, Registry: reg}

	svc.leaseActorBinding("s1", "")

	bindings, err := reg.ListBindingsForTarget(context.Background(), registry.LogicalAgentBindingTarget(""))
	if err != nil {
		t.Fatalf("list bindings: %v", err)
	}
	if len(bindings) != 0 {
		t.Fatalf("expected no binding to be leased for an empty logical agent id, got %d", len(bindings))
	}
}

func TestLeaseActorBinding_CreatesResolvableBinding(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "lease.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	reg := registry.NewService(registry.NewStorage(st.DB()))
	svc := &Service{Store: st, Registry: reg}

	svc.leaseActorBinding("s1", "worker")

	b, err := reg.CurrentBinding(context.Background(), registry.LogicalAgentBindingTarget("worker"))
	if err != nil {
		t.Fatalf("current binding: %v", err)
	}
	if b.SessionID != "s1" || b.HostID != localHostID {
		t.Fatalf("binding = %+v, want SessionID=s1 HostID=%s", b, localHostID)
	}
}

func TestRevokeActorBindingIfCurrent_RevokesOwnBinding(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "revoke.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	reg := registry.NewService(registry.NewStorage(st.DB()))
	svc := &Service{Store: st, Registry: reg}

	svc.leaseActorBinding("s1", "worker")
	svc.revokeActorBindingIfCurrent("s1", "worker")

	if _, err := reg.CurrentBinding(context.Background(), registry.LogicalAgentBindingTarget("worker")); err == nil {
		t.Fatalf("expected no current binding after revoke")
	}
}

// TestRevokeActorBindingIfCurrent_DoesNotRevokeNewerSession is the fencing
// hygiene case: a delayed stop call for a session already superseded by a
// newer launch must never touch the newer session's active binding.
func TestRevokeActorBindingIfCurrent_DoesNotRevokeNewerSession(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "revoke-race.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	reg := registry.NewService(registry.NewStorage(st.DB()))
	svc := &Service{Store: st, Registry: reg}

	svc.leaseActorBinding("s1", "worker")
	svc.leaseActorBinding("s2", "worker") // supersedes s1's binding

	svc.revokeActorBindingIfCurrent("s1", "worker") // delayed stop for the OLD session

	b, err := reg.CurrentBinding(context.Background(), registry.LogicalAgentBindingTarget("worker"))
	if err != nil {
		t.Fatalf("current binding: %v", err)
	}
	if b.SessionID != "s2" {
		t.Fatalf("current binding session = %q, want %q (s1's delayed stop must not revoke s2's active binding)", b.SessionID, "s2")
	}
}
