package e2e

// registry_test.go — T11 "binding revocation" and "groups/replacements"
// scenarios, promoted to a real running daemon process. In-process
// coverage already exists for both (internal/api/bindings_test.go,
// internal/registry/group_fanout_test.go); this proves the same
// guarantees hold through the real public HTTP surface, not just
// httptest.NewServer.

import (
	"encoding/json"
	"testing"

	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/registry"
)

func TestRegistry_BindingRevocation_RealDaemon(t *testing.T) {
	d := StartFixtureDaemon(t)
	c := d.Client()

	agentURN := registerAgent(t, c, "e2e-bindable-agent")

	binding, err := c.Bindings().Lease(ctx(), agentURN, "sess-1", "host-1", "attempt-1", []string{"pull-only"}, 60)
	if err != nil {
		t.Fatalf("lease: %v", err)
	}

	current, err := c.Bindings().Current(ctx(), agentURN)
	if err != nil {
		t.Fatalf("current before revoke: %v", err)
	}
	if current.ID != binding.ID {
		t.Fatalf("current binding id = %q, want %q", current.ID, binding.ID)
	}

	if err := c.Bindings().Revoke(ctx(), binding.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	if _, err := c.Bindings().Current(ctx(), agentURN); err == nil {
		t.Fatal("expected an error resolving Current after revocation, got nil (revoked binding must not still be current)")
	}

	// Idempotent: revoking an already-revoked binding must not error.
	if err := c.Bindings().Revoke(ctx(), binding.ID); err != nil {
		t.Fatalf("second revoke (idempotent): %v", err)
	}

	// A NEW lease after revocation must succeed and become current --
	// revocation must not permanently wedge the target.
	second, err := c.Bindings().Lease(ctx(), agentURN, "sess-2", "host-1", "attempt-2", []string{"pull-only"}, 60)
	if err != nil {
		t.Fatalf("lease after revoke: %v", err)
	}
	current2, err := c.Bindings().Current(ctx(), agentURN)
	if err != nil {
		t.Fatalf("current after re-lease: %v", err)
	}
	if current2.ID != second.ID {
		t.Fatalf("current binding after re-lease = %q, want %q", current2.ID, second.ID)
	}
}

func TestRegistry_GroupMembershipReplacement_RealDaemon(t *testing.T) {
	d := StartFixtureDaemon(t)
	c := d.Client()

	owner := registerAgent(t, c, "e2e-group-owner")
	bob := registerAgent(t, c, "e2e-group-bob")
	carol := registerAgent(t, c, "e2e-group-carol")

	group, err := c.Groups().Create(ctx(), client.CreateGroupRequest{DisplayName: "e2e-fanout-group", CreatorURN: owner})
	if err != nil {
		t.Fatalf("create group: %v", err)
	}
	if _, err := c.Groups().AddMember(ctx(), group.URN, bob, owner, registry.MemberRoleMember); err != nil {
		t.Fatalf("add bob: %v", err)
	}

	payload, err := json.Marshal(map[string]string{"text": "hi bob"})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	sendResult, err := c.Groups().Send(ctx(), group.URN, client.SendGroupRequest{From: owner, Payload: payload})
	if err != nil {
		t.Fatalf("send to group: %v", err)
	}

	// Replace membership: remove bob, add carol -- AFTER the send above.
	if err := c.Groups().RemoveMember(ctx(), group.URN, bob, owner); err != nil {
		t.Fatalf("remove bob: %v", err)
	}
	if _, err := c.Groups().AddMember(ctx(), group.URN, carol, owner, registry.MemberRoleMember); err != nil {
		t.Fatalf("add carol: %v", err)
	}

	// The room-level message list is membership-independent: it must
	// still show the original message regardless of who's a member now.
	msgs, err := c.Groups().ListMessages(ctx(), group.URN, client.ListMessagesParams{As: owner})
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	var found bool
	for _, m := range msgs.Messages {
		if m.ID == sendResult.MessageID {
			found = true
		}
	}
	if !found {
		t.Fatalf("group message %s missing after membership replacement", sendResult.MessageID)
	}

	members, err := c.Groups().ListMembers(ctx(), group.URN)
	if err != nil {
		t.Fatalf("list members: %v", err)
	}
	var haveCarol, haveBob bool
	for _, m := range members {
		if m.MemberURN == carol {
			haveCarol = true
		}
		if m.MemberURN == bob {
			haveBob = true
		}
	}
	if !haveCarol {
		t.Error("carol missing from membership after replacement")
	}
	if haveBob {
		t.Error("bob still present after being removed")
	}
}
