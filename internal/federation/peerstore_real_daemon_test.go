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

// TestPeerDaemon_CrossAuthorityAccessFailure_Is403AndObservable is T07's
// "cross-authority access failures are observable" acceptance case.
//
// A distinct review pass correctly flagged that a first draft of this
// test routed through httpPeerStore.Get -- which structurally can never
// send a WRONG ?as= (the shared messaging.Store interface's Get(ctx, id)
// carries no identity parameter at all, so peerstore.go never sends `as`
// for Get), meaning it could only ever observe the weaker "no claim at
// all" 400 path (internal/api/messages.go's handleMessageGet requires
// `as`) rather than the actual cross-authority 403 rejection
// (`as` present but wrong) -- a broken authorization check on that 403
// branch would NOT have been caught by that version.
//
// This version issues a raw HTTP GET with a present-but-wrong ?as=
// directly against the real peer daemon (the same internal/api.Server a
// federated hop ultimately talks to), so it actually exercises and
// verifies the 403 branch itself, not a different, weaker one.
func TestPeerDaemon_CrossAuthorityAccessFailure_Is403AndObservable(t *testing.T) {
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
