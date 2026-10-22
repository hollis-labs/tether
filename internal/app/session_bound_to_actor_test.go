package app

import (
	"context"
	"testing"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/registry"
)

// CW-20261021-0029: an inbox pull treats a session as the recipient of an actor's mailbox only
// when the actor's CURRENT binding resolves to exactly that session; nothing looser.
func TestSessionBoundToActor(t *testing.T) {
	ctx := context.Background()
	target := registry.LogicalAgentBindingTarget("agt_x")
	lease := func(reg *registry.Service, session string, caps []string, vis registry.PublicationVisibility) {
		t.Helper()
		if _, err := reg.LeaseBinding(ctx, target, session, "local", session, caps, vis, 0); err != nil {
			t.Fatal(err)
		}
	}
	setup := func(t *testing.T) (*Service, *registry.Service) {
		_, reg := newWakeHarness(t)
		return &Service{Registry: reg}, reg
	}

	t.Run("bound to this session", func(t *testing.T) {
		s, reg := setup(t)
		lease(reg, "s1", nil, "")
		if !s.SessionBoundToActor("s1", "agt_x") {
			t.Fatal("the session the actor is bound to must be its recipient")
		}
	})
	t.Run("a session bound to a different actor is not the recipient", func(t *testing.T) {
		s, reg := setup(t)
		lease(reg, "s1", nil, "")
		if _, err := reg.LeaseBinding(ctx, registry.LogicalAgentBindingTarget("agt_other"), "s2", "local", "s2", nil, "", 0); err != nil {
			t.Fatal(err)
		}
		if s.SessionBoundToActor("s2", "agt_x") {
			t.Fatal("s2 is bound to agt_other, not agt_x")
		}
		if s.SessionBoundToActor("s1", "agt_other") {
			t.Fatal("s1 is bound to agt_x, not agt_other")
		}
	})
	t.Run("unbound session and unbound actor", func(t *testing.T) {
		s, reg := setup(t)
		if s.SessionBoundToActor("s1", "agt_x") {
			t.Fatal("an actor with no binding has no recipient session")
		}
		lease(reg, "s1", nil, "")
		if s.SessionBoundToActor("s-unbound", "agt_x") {
			t.Fatal("a session with no binding is not the recipient")
		}
	})
	t.Run("a superseded binding is not the recipient, the new one is", func(t *testing.T) {
		s, reg := setup(t)
		lease(reg, "s1", nil, "")
		lease(reg, "s2", nil, "")
		if s.SessionBoundToActor("s1", "agt_x") || !s.SessionBoundToActor("s2", "agt_x") {
			t.Fatal("only the current generation's session is the recipient")
		}
	})
	t.Run("a pull-only bridge binding is never a recipient", func(t *testing.T) {
		s, reg := setup(t)
		lease(reg, "s1", []string{api.PullOnlyCapability}, registry.VisibilityPublishedLocal)
		if s.SessionBoundToActor("s1", "agt_x") {
			t.Fatal("a published-local bridge is not a Tether-managed recipient session")
		}
	})
	t.Run("a revoked binding is not the recipient", func(t *testing.T) {
		s, reg := setup(t)
		lease(reg, "s1", nil, "")
		if _, err := reg.RevokeSessionBindings(ctx, "s1"); err != nil {
			t.Fatal(err)
		}
		if s.SessionBoundToActor("s1", "agt_x") {
			t.Fatal("a revoked binding must not make the session the recipient")
		}
	})
	t.Run("no registry or empty ids", func(t *testing.T) {
		if (&Service{}).SessionBoundToActor("s1", "agt_x") {
			t.Fatal("no registry, no binding")
		}
		s, reg := setup(t)
		lease(reg, "s1", nil, "")
		if s.SessionBoundToActor("", "agt_x") || s.SessionBoundToActor("s1", "") {
			t.Fatal("empty ids are never bound")
		}
	})
}
