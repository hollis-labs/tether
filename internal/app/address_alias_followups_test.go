package app

import (
	"context"
	"errors"
	"testing"

	"github.com/hollis-labs/libs/harness/core/adapters/agentsessions"
	messaging "github.com/hollis-labs/libs/mesh/core/messaging"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/registry"
)

// CW-20261114-0068: the routed-reply policy follows address aliases.
func TestReplyPolicyFollowsAddressAliases(t *testing.T) {
	st, _ := newWakeHarness(t)
	ctx := context.Background()
	const canon, old = "msg://agent/test/canon-person", "msg://agent/test/old-person"
	if _, _, err := st.AddAddressAlias(ctx, old, canon, "test"); err != nil {
		t.Fatal(err)
	}
	from, _ := messaging.ParseURN("msg://agent/test/sender")
	to, _ := messaging.ParseURN(canon)
	parent := messaging.Envelope{From: from, To: to}
	policy := NewIdentityPolicy(identity.Enforce, nil)
	policy.AddressForms = st.AddressForms
	reply := policy.Reply()

	// A principal that owns the old form is a participant of mail to the canonical one.
	if err := reply(ctx, verifiedPrincipal(old), parent, ""); err != nil {
		t.Fatalf("alias owner: %v", err)
	}
	if err := reply(ctx, verifiedPrincipal(strangerID), parent, ""); !errors.Is(err, ErrNotParticipant) {
		t.Fatalf("stranger: %v", err)
	}
	// Without the alias hook the comparison is literal, as before.
	literal := NewIdentityPolicy(identity.Enforce, nil).Reply()
	if err := literal(ctx, verifiedPrincipal(old), parent, ""); !errors.Is(err, ErrNotParticipant) {
		t.Fatalf("literal policy: %v", err)
	}
}

// CW-20261114-0068: a session bound to an alias is woken for mail stored under
// the canonical address, and a delivery owed to an alias reaches the canonical
// address's session.
func TestResolveWakeTargetFollowsAddressAliases(t *testing.T) {
	st, reg := newWakeHarness(t)
	ctx := context.Background()
	rt := newFakeRuntime()
	rt.setAlive("s-old", true, agentsessions.LiveStateIdle)
	if _, err := reg.LeaseBinding(ctx, registry.LogicalAgentBindingTarget("old-seat"), "s-old", "local", "s-old", nil, "", 0); err != nil {
		t.Fatal(err)
	}
	canon := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "canon-seat"}
	old := messaging.Address{Kind: messaging.KindAgent, Authority: "test", ID: "old-seat"}
	if got, _ := resolveWakeTarget(ctx, st, reg, rt.seam(), canon); got != "" {
		t.Fatalf("before the alias: %q", got)
	}
	if _, _, err := st.AddAddressAlias(ctx, old.URN(), canon.URN(), "test"); err != nil {
		t.Fatal(err)
	}
	if got, err := resolveWakeTarget(ctx, st, reg, rt.seam(), canon); got != "s-old" || err != nil {
		t.Fatalf("canonical mail, session bound to the alias: %q, %v", got, err)
	}
}
