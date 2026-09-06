package federation

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

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

// TestRouterMutualPeerConfiguration_NoLoop is T07's "routing avoids
// loops/echo duplicates" acceptance case: two Routers that each treat the
// OTHER as its one peer must not ping-pong an envelope -- each Send is a
// single, terminal hop (see router.go's "Loop/echo safety" doc). Both
// directions are exercised on a bounded timeout so an actual infinite
// recursion/hang would fail the test rather than run forever.
func TestRouterMutualPeerConfiguration_NoLoop(t *testing.T) {
	storeA := memstore.New()
	storeB := memstore.New()
	routerA := NewRouter(storeA, "a")
	routerB := NewRouter(storeB, "b")
	if err := routerA.Register("b", routerB); err != nil {
		t.Fatalf("register b on a: %v", err)
	}
	if err := routerB.Register("a", routerA); err != nil {
		t.Fatalf("register a on b: %v", err)
	}

	done := make(chan error, 2)
	go func() {
		_, err := routerA.Send(context.Background(), envTo(addr("b", "worker")))
		done <- err
	}()
	go func() {
		_, err := routerB.Send(context.Background(), envTo(addr("a", "worker")))
		done <- err
	}()
	for i := 0; i < 2; i++ {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Send: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("Send did not return -- possible routing loop between mutually-configured peers")
		}
	}

	// Each envelope landed exactly once, in the OTHER store -- a single
	// terminal hop, not an echo back to the sender's own authority.
	bInbox, err := storeB.Inbox(context.Background(), addr("b", "worker"), messaging.Filter{})
	if err != nil || len(bInbox) != 1 {
		t.Fatalf("storeB inbox = %v, err=%v, want exactly 1 envelope", bInbox, err)
	}
	aInbox, err := storeA.Inbox(context.Background(), addr("a", "worker"), messaging.Filter{})
	if err != nil || len(aInbox) != 1 {
		t.Fatalf("storeA inbox = %v, err=%v, want exactly 1 envelope", aInbox, err)
	}
}
