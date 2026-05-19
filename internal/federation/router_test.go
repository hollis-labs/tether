package federation

import (
	"context"
	"errors"
	"reflect"
	"testing"

	messaging "github.com/hollis-labs/go-messaging"
	"github.com/hollis-labs/go-messaging/memstore"
)

func addr(authority, id string) messaging.Address {
	return messaging.Address{Kind: messaging.KindAgent, Authority: authority, ID: id}
}

func envTo(to messaging.Address) messaging.Envelope {
	return messaging.Envelope{
		Kind: messaging.MsgKindNotice,
		From: addr("tether", "sender"),
		To:   to,
	}
}

func TestRouterRoutesSendByRecipientAuthority(t *testing.T) {
	ctx := context.Background()
	local := memstore.New()
	peer := memstore.New()

	r := NewRouter(local, "tether")
	if err := r.Register("torque", peer); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// A message to the peer authority lands in the peer store only.
	sent, err := r.Send(ctx, envTo(addr("torque", "worker")))
	if err != nil {
		t.Fatalf("Send to peer: %v", err)
	}
	if _, err := peer.Get(ctx, sent.ID); err != nil {
		t.Fatalf("peer store missing the routed envelope: %v", err)
	}
	if _, err := local.Get(ctx, sent.ID); !errors.Is(err, messaging.ErrNotFound) {
		t.Fatalf("local store should not hold a peer-routed envelope, got %v", err)
	}

	// A message to the local authority lands in the local store only.
	sentLocal, err := r.Send(ctx, envTo(addr("tether", "worker")))
	if err != nil {
		t.Fatalf("Send to local: %v", err)
	}
	if _, err := local.Get(ctx, sentLocal.ID); err != nil {
		t.Fatalf("local store missing the local envelope: %v", err)
	}
	if _, err := peer.Get(ctx, sentLocal.ID); !errors.Is(err, messaging.ErrNotFound) {
		t.Fatalf("peer store should not hold a local envelope, got %v", err)
	}
}

func TestRouterUnknownAuthorityFallsThroughToLocal(t *testing.T) {
	ctx := context.Background()
	local := memstore.New()
	r := NewRouter(local, "tether")

	// No peer for "elsewhere" — a non-strict router serves it locally.
	sent, err := r.Send(ctx, envTo(addr("elsewhere", "x")))
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if _, err := local.Get(ctx, sent.ID); err != nil {
		t.Fatalf("unknown authority should fall through to local: %v", err)
	}
}

func TestRouterStrictRejectsUnknownAuthority(t *testing.T) {
	ctx := context.Background()
	r := NewRouter(memstore.New(), "tether", WithStrictRouting())

	_, err := r.Send(ctx, envTo(addr("elsewhere", "x")))
	if !errors.Is(err, ErrNoRoute) {
		t.Fatalf("strict Send to unknown authority: got %v, want ErrNoRoute", err)
	}
	// The local authority still routes fine under strict mode.
	if _, err := r.Send(ctx, envTo(addr("tether", "x"))); err != nil {
		t.Fatalf("strict Send to local authority: %v", err)
	}
}

func TestRouterInboxAndConsumeRouteByRecipient(t *testing.T) {
	ctx := context.Background()
	local := memstore.New()
	peer := memstore.New()
	r := NewRouter(local, "tether")
	if err := r.Register("torque", peer); err != nil {
		t.Fatalf("Register: %v", err)
	}

	to := addr("torque", "worker")
	sent, err := r.Send(ctx, envTo(to))
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	got, err := r.Inbox(ctx, to, messaging.Filter{})
	if err != nil {
		t.Fatalf("Inbox: %v", err)
	}
	if len(got) != 1 || got[0].ID != sent.ID {
		t.Fatalf("Inbox routed wrong: got %d envelopes, want the peer one", len(got))
	}
	if err := r.Consume(ctx, sent.ID, to); err != nil {
		t.Fatalf("Consume routed to peer: %v", err)
	}
}

func TestRouterIDKeyedOpsTargetLocal(t *testing.T) {
	ctx := context.Background()
	local := memstore.New()
	peer := memstore.New()
	r := NewRouter(local, "tether")
	if err := r.Register("torque", peer); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// An envelope sent straight into the local store...
	sent, err := local.Send(ctx, envTo(addr("tether", "x")))
	if err != nil {
		t.Fatalf("local Send: %v", err)
	}
	// ...is reachable via the Router's ID-keyed Get (which targets local).
	if _, err := r.Get(ctx, sent.ID); err != nil {
		t.Fatalf("Router.Get should serve from local store: %v", err)
	}
	if err := r.Cancel(ctx, sent.ID); err != nil {
		t.Fatalf("Router.Cancel should serve from local store: %v", err)
	}
}

func TestRouterRegistry(t *testing.T) {
	local := memstore.New()
	r := NewRouter(local, "tether")

	if !r.IsLocal("tether") || !r.IsLocal("anything-without-a-peer") {
		t.Fatal("IsLocal should be true for the local authority and unrouted authorities")
	}
	if err := r.Register("torque", memstore.New()); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if r.IsLocal("torque") {
		t.Fatal("IsLocal should be false for a registered peer")
	}
	if got := r.Authorities(); !reflect.DeepEqual(got, []string{"torque"}) {
		t.Fatalf("Authorities() = %v, want [torque]", got)
	}
	r.Unregister("torque")
	if !r.IsLocal("torque") || len(r.Authorities()) != 0 {
		t.Fatal("Unregister should drop the peer route")
	}
}

func TestRouterRegisterRejectsBadInput(t *testing.T) {
	r := NewRouter(memstore.New(), "tether")
	if err := r.Register("", memstore.New()); err == nil {
		t.Fatal("Register with empty authority should fail")
	}
	if err := r.Register("torque", nil); err == nil {
		t.Fatal("Register with nil store should fail")
	}
	if err := r.Register("tether", memstore.New()); err == nil {
		t.Fatal("Register of the local authority should fail")
	}
}

// Router is itself a Store, so it composes with go-messaging's Dispatcher.
func TestRouterSatisfiesStore(t *testing.T) {
	var _ messaging.Store = NewRouter(memstore.New(), "tether")
}
