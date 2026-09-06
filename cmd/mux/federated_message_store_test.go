package main

// federated_message_store_test.go — T05 (messaging vNext, CW-20260906-0036).
// Proves the fix for the gap ADR-0040 itself documented: the federation
// Router was fully built and tested but never actually invoked by the
// running daemon (cmd/mux/daemon.go wired the bare local store into
// api.Server.MessageStore, not svc.Federation).

import (
	"context"
	"path/filepath"
	"testing"

	gomsg "github.com/hollis-labs/go-messaging"
	"github.com/hollis-labs/go-messaging/memstore"

	"github.com/hollis-labs/tether/internal/federation"
	"github.com/hollis-labs/tether/internal/store"
)

func openLocalStoreForTest(t *testing.T) store.InboxStore {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "federated.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db.MessagingStore()
}

func TestNewFederatedMessageStore_NilRouterReturnsLocalUnchanged(t *testing.T) {
	local := openLocalStoreForTest(t)
	got := newFederatedMessageStore(local, nil)
	if got != local {
		// Comparing interface values holding the same concrete pointer.
		t.Fatalf("expected the standalone (non-federated) default to return local unchanged, got a different value")
	}
}

func TestNewFederatedMessageStore_SendRoutesToPeerAuthority(t *testing.T) {
	local := openLocalStoreForTest(t)
	peer := memstore.New()
	router := federation.NewRouter(local, "home")
	if err := router.Register("away", peer); err != nil {
		t.Fatalf("register peer: %v", err)
	}

	fs := newFederatedMessageStore(local, router)
	ctx := context.Background()

	from := gomsg.Address{Kind: gomsg.KindAgent, Authority: "home", ID: "alice"}
	toPeer := gomsg.Address{Kind: gomsg.KindAgent, Authority: "away", ID: "bob"}
	sent, err := fs.Send(ctx, gomsg.Envelope{From: from, To: toPeer, Kind: gomsg.MsgKindNotice})
	if err != nil {
		t.Fatalf("send to peer authority: %v", err)
	}

	// Landed in the PEER store, not local -- this is the actual proof the
	// Router is really being invoked, not just constructed and ignored.
	if _, err := peer.Get(ctx, sent.ID); err != nil {
		t.Fatalf("expected the message to land in the peer store: %v", err)
	}
	if _, err := local.Get(ctx, sent.ID); err == nil {
		t.Fatalf("expected the message to NOT land in the local store when addressed to a routed peer authority")
	}

	// A local-authority send still lands locally.
	toLocal := gomsg.Address{Kind: gomsg.KindAgent, Authority: "home", ID: "carol"}
	sentLocal, err := fs.Send(ctx, gomsg.Envelope{From: from, To: toLocal, Kind: gomsg.MsgKindNotice})
	if err != nil {
		t.Fatalf("send to local authority: %v", err)
	}
	if _, err := local.Get(ctx, sentLocal.ID); err != nil {
		t.Fatalf("expected a local-authority send to land in the local store: %v", err)
	}
}

func TestNewFederatedMessageStore_InboxSubscribeConsumeRouteByAuthority(t *testing.T) {
	local := openLocalStoreForTest(t)
	peer := memstore.New()
	router := federation.NewRouter(local, "home")
	if err := router.Register("away", peer); err != nil {
		t.Fatalf("register peer: %v", err)
	}
	fs := newFederatedMessageStore(local, router)
	ctx := context.Background()

	recipient := gomsg.Address{Kind: gomsg.KindAgent, Authority: "away", ID: "bob"}
	sent, err := peer.Send(ctx, gomsg.Envelope{
		From: gomsg.Address{Kind: gomsg.KindAgent, Authority: "away", ID: "alice"},
		To:   recipient, Kind: gomsg.MsgKindNotice,
	})
	if err != nil {
		t.Fatalf("seed peer send: %v", err)
	}

	// Inbox for an "away" recipient must be served by the peer store via
	// the router, not the (empty) local store.
	envs, err := fs.Inbox(ctx, recipient, gomsg.Filter{})
	if err != nil {
		t.Fatalf("inbox: %v", err)
	}
	if len(envs) != 1 || envs[0].ID != sent.ID {
		t.Fatalf("expected the routed inbox to return the peer-store message, got %+v", envs)
	}

	// Consume likewise routes to the peer by recipient authority.
	if err := fs.Consume(ctx, sent.ID, recipient); err != nil {
		t.Fatalf("consume: %v", err)
	}
	got, err := peer.Get(ctx, sent.ID)
	if err != nil {
		t.Fatalf("get from peer after consume: %v", err)
	}
	if got.ConsumedAt == nil {
		t.Fatalf("expected the peer store's copy to be marked consumed via the routed Consume call")
	}
}

func TestNewFederatedMessageStore_ListMarkReadStayLocalEvenWhenFederated(t *testing.T) {
	// List/MarkRead/Archive/Unarchive are InboxStore's superset -- Router
	// doesn't implement them at all (it only satisfies the 7-method
	// messaging.Store), so they must always come from local regardless of
	// federation configuration. This is what makes newFederatedMessageStore
	// a composition, not a drop-in Router substitution (confirmed against
	// the actual Router source before implementing).
	local := openLocalStoreForTest(t)
	router := federation.NewRouter(local, "home")
	fs := newFederatedMessageStore(local, router)
	ctx := context.Background()

	from := gomsg.Address{Kind: gomsg.KindAgent, Authority: "home", ID: "alice"}
	to := gomsg.Address{Kind: gomsg.KindAgent, Authority: "home", ID: "bob"}
	sent, err := fs.Send(ctx, gomsg.Envelope{From: from, To: to, Kind: gomsg.MsgKindNotice})
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	page, err := fs.List(ctx, to, store.ListFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(page.Messages) != 1 || page.Messages[0].ID != sent.ID {
		t.Fatalf("expected List to serve the local message directly, got %+v", page.Messages)
	}
	if err := fs.MarkRead(ctx, sent.ID, to); err != nil {
		t.Fatalf("mark read: %v", err)
	}
}
