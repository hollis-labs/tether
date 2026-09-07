package federation_test

// peerstore_real_daemon_test.go — T07 (messaging vNext, CW-20260906-0038).
// fakeDaemon (peerstore_test.go, internal package federation) is a
// hand-rolled stand-in that never enforced ?as= on /messages/inbox the way
// the REAL internal/api handler has since T05 (ADR 0045) -- so it could
// not have caught, and did not catch, a real regression: httpPeerStore.
// Inbox never sent ?as=, meaning every federated Inbox call has 400'd
// against an actual Tether peer since T05 landed. This file spins up the
// real internal/api.Server (the same one a genuine peer daemon runs) to
// close that blind spot and prove the fix (peerstore.go's Inbox now sends
// as=to.URN(), mirroring internal/client.Client.MessageInbox's identical
// convention).
//
// This lives in an EXTERNAL test package (federation_test, not federation)
// because internal/api transitively imports internal/config, which
// imports internal/federation -- an internal test file (package
// federation) importing internal/api would be a real import cycle; an
// external test package is a separate compilation unit and is not.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	messaging "github.com/hollis-labs/go-messaging"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/federation"
	"github.com/hollis-labs/tether/internal/store"
)

func realDaemon(t *testing.T) *httptest.Server {
	t.Helper()
	db, err := store.Open(t.TempDir() + "/peer.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	srv := httptest.NewServer(api.NewHandler(api.Deps{MessageStore: db.MessagingStore()}))
	t.Cleanup(srv.Close)
	return srv
}

func TestPeerStoreInbox_AgainstRealDaemon_RequiredAsIsSent(t *testing.T) {
	ctx := context.Background()
	srv := realDaemon(t)
	ps, err := federation.HTTPDialer(nil)(federation.Peer{Authority: "torque", BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("HTTPDialer: %v", err)
	}

	to := messaging.Address{Kind: messaging.KindAgent, Authority: "torque", ID: "worker"}
	env := messaging.Envelope{Kind: messaging.MsgKindNotice, From: messaging.Address{Kind: messaging.KindAgent, Authority: "tether", ID: "sender"}, To: to}
	if _, err := ps.Send(ctx, env); err != nil {
		t.Fatalf("Send: %v", err)
	}

	inbox, err := ps.Inbox(ctx, to, messaging.Filter{})
	if err != nil {
		t.Fatalf("Inbox against a real daemon: %v (peerstore.go must send ?as= matching ?to=, per ADR 0045 / T05)", err)
	}
	if len(inbox) != 1 {
		t.Fatalf("Inbox returned %d envelopes, want 1", len(inbox))
	}
}

// TestRealDaemon_MessageGet_RejectsPresentButWrongAs verifies the real
// internal/api.Server (not the federation package's hand-rolled fakeDaemon
// stand-in) rejects a present-but-wrong ?as= with 403 on GET /messages/{id}
// (T05, ADR 0045).
//
// A distinct review pass on T07 correctly flagged that an earlier version
// of this test was mislabeled as a FEDERATION acceptance case ("cross-
// authority access failure... observable across a hop") when it neither
// routes through httpPeerStore/Router nor could: messaging.Store's
// Get(ctx, id) carries no caller identity at all, so peerstore.go's Get
// now refuses outright (ErrNoIdentityToAssert) rather than ever sending a
// wrong -- or any -- ?as=. Router.Get also always resolves locally by
// design (ids carry no authority to route on; see router.go), so a Get
// request structurally never crosses a federation hop in the first place.
// This test verifies real, valuable behavior (the actual production
// handler's authorization, not the hand-rolled fake's), but it is a plain
// internal/api integration check, not federation/T07 evidence. See
// TestRealDaemon_FederatedConsume_WrongRecipientObservableAcrossHop below
// for the genuine federation-routed "cross-boundary failure is observable"
// proof.
func TestRealDaemon_MessageGet_RejectsPresentButWrongAs(t *testing.T) {
	ctx := context.Background()
	srv := realDaemon(t)
	ps, err := federation.HTTPDialer(nil)(federation.Peer{Authority: "torque", BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("HTTPDialer: %v", err)
	}

	to := messaging.Address{Kind: messaging.KindAgent, Authority: "torque", ID: "real-owner"}
	env := messaging.Envelope{Kind: messaging.MsgKindNotice, From: messaging.Address{Kind: messaging.KindAgent, Authority: "tether", ID: "sender"}, To: to}
	sent, err := ps.Send(ctx, env)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	imposter := messaging.Address{Kind: messaging.KindAgent, Authority: "torque", ID: "imposter"}
	resp, err := http.Get(srv.URL + "/messages/" + sent.ID + "?as=" + imposter.URN())
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (a present-but-wrong ?as= must be rejected, not silently allowed)", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "sender or recipient") {
		t.Fatalf("body = %s, want a descriptive rejection naming the actual cross-authority reason, not a generic/unlabeled failure", body)
	}
}

// TestRealDaemon_FederatedConsume_WrongRecipientObservableAcrossHop is the
// genuine federation-routed proof for T07's "cross-authority access
// failure... observable" acceptance case: unlike Get/Thread (which never
// cross a hop at all, see above), Consume DOES route across a real
// federation hop by recipient.Authority (router.go), and its recipient
// address is exactly what peerstore.go asserts as ?as= -- so a caller
// addressing the wrong recipient produces a genuine, peer-daemon-enforced
// 409 that must propagate back through httpPeerStore as a distinguishable
// error, not a swallowed generic failure or a false success.
func TestRealDaemon_FederatedConsume_WrongRecipientObservableAcrossHop(t *testing.T) {
	ctx := context.Background()
	srv := realDaemon(t)
	peerStore, err := federation.HTTPDialer(nil)(federation.Peer{Authority: "torque", BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("HTTPDialer: %v", err)
	}
	local, err := store.Open(t.TempDir() + "/local.db")
	if err != nil {
		t.Fatalf("open local store: %v", err)
	}
	t.Cleanup(func() { local.Close() })
	r := federation.NewRouter(local.MessagingStore(), "tether")
	if err := r.Register("torque", peerStore); err != nil {
		t.Fatalf("Register: %v", err)
	}

	to := messaging.Address{Kind: messaging.KindAgent, Authority: "torque", ID: "real-owner"}
	env := messaging.Envelope{Kind: messaging.MsgKindNotice, From: messaging.Address{Kind: messaging.KindAgent, Authority: "tether", ID: "sender"}, To: to}
	sent, err := r.Send(ctx, env)
	if err != nil {
		t.Fatalf("Send (routed to the peer authority): %v", err)
	}

	imposter := messaging.Address{Kind: messaging.KindAgent, Authority: "torque", ID: "imposter"}
	err = r.Consume(ctx, sent.ID, imposter)
	if !errors.Is(err, federation.ErrWrongRecipient) {
		t.Fatalf("Consume across the real federation hop as the wrong recipient: got %v, want federation.ErrWrongRecipient (observable, distinguishable -- not a generic 500 or a silent success)", err)
	}

	// The genuine recipient can still consume it -- the failure above was
	// real authorization, not a broken/wedged delivery.
	if err := r.Consume(ctx, sent.ID, to); err != nil {
		t.Fatalf("Consume as the real recipient: %v", err)
	}
}
