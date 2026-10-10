package federation_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	messaging "github.com/hollis-labs/substrate/mesh/messaging"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/daemon"
	"github.com/hollis-labs/tether/internal/federation"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/store"
)

type peerFixtureCatalog struct{}

func (peerFixtureCatalog) Load() (*config.Catalog, error) { return &config.Catalog{}, nil }

// Match the CLI composition seam: the four routable methods use Router;
// the extra local InboxStore methods retain the actual local implementation.
type peerFixtureMessages struct {
	store.InboxStore
	router *federation.Router
}

func (s *peerFixtureMessages) Send(ctx context.Context, e messaging.Envelope) (messaging.Envelope, error) {
	return s.router.Send(ctx, e)
}
func (s *peerFixtureMessages) Inbox(ctx context.Context, to messaging.Address, f messaging.Filter) ([]messaging.Envelope, error) {
	return s.router.Inbox(ctx, to, f)
}
func (s *peerFixtureMessages) Subscribe(ctx context.Context, to messaging.Address, f messaging.Filter) (<-chan messaging.Envelope, error) {
	return s.router.Subscribe(ctx, to, f)
}
func (s *peerFixtureMessages) Consume(ctx context.Context, id string, to messaging.Address) error {
	return s.router.Consume(ctx, id, to)
}

func authPeerDB(t *testing.T) (*store.Store, *daemon.Server) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := &daemon.Server{Config: daemon.Config{IdentityMode: identity.Off}, Identity: identity.NewStore(db.DB()), MessageStore: db.MessagingStore(), Catalog: peerFixtureCatalog{}}
	t.Cleanup(s.CloseIdentityAudit)
	return db, s
}

func syntheticPeerDevice(t *testing.T, ids *identity.Store, scopes []string) identity.DeviceExchange {
	t.Helper()
	op := identity.Principal{ID: identity.OperatorID, Kind: "operator"}
	grant, err := ids.CreatePairingGrant(context.Background(), op, "synthetic federation fixture", scopes, time.Minute, "")
	if err != nil {
		t.Fatal(err)
	}
	d, err := ids.ExchangePairingGrant(context.Background(), grant.Code, scopes, "")
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func syntheticPeerReference(t *testing.T, token string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "peer.token")
	if err := os.WriteFile(path, []byte(token+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return "file://" + path
}

func TestAuthenticatedFederationAcrossTwoDaemonHandlers(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	// Two actual daemon HTTP compositions, independent stores and loopback
	// listeners. No process/service, provider, SSH or real credential is used.
	receiverDB, receiver := authPeerDB(t)
	receiverHTTP := httptest.NewUnstartedServer(nil)
	receiverHTTP.Config.Handler = receiver.RemoteHandler(receiverHTTP.Listener.Addr().String())
	receiverHTTP.Start()
	t.Cleanup(receiverHTTP.Close)
	device := syntheticPeerDevice(t, receiver.Identity, []string{identity.ScopeRead, identity.ScopeOperate})
	ref := syntheticPeerReference(t, device.Token)
	peer := federation.Peer{Authority: "worker", BaseURL: receiverHTTP.URL, CredentialRef: ref}
	senderDB, sender := authPeerDB(t)
	router, err := federation.BuildRouter(federation.Config{Enabled: true, LocalAuthority: "home", Strict: true, Peers: []federation.Peer{peer}}, senderDB.MessagingStore(), federation.HTTPDialer(nil))
	if err != nil {
		t.Fatal(err)
	}
	sender.MessageStore = &peerFixtureMessages{InboxStore: senderDB.MessagingStore(), router: router}
	senderHTTP := httptest.NewServer(sender.Handler())
	t.Cleanup(senderHTTP.Close)
	client, err := federation.HTTPDialer(nil)(federation.Peer{Authority: "home", BaseURL: senderHTTP.URL})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	to := messaging.Address{Kind: messaging.KindAgent, Authority: "worker", ID: "recipient"}
	from := messaging.Address{Kind: messaging.KindAgent, Authority: "home", ID: "sender"}
	streamCtx, stopStream := context.WithCancel(ctx)
	defer stopStream()
	stream, err := client.Subscribe(streamCtx, to, messaging.Filter{})
	if err != nil {
		t.Fatalf("Subscribe through real daemon remote scope/auth and required as: %v", err)
	}
	sent, err := client.Send(ctx, messaging.Envelope{Kind: messaging.MsgKindNotice, From: from, To: to})
	if err != nil {
		t.Fatal("Send", err)
	}
	select {
	case got, ok := <-stream:
		if !ok || got.ID != sent.ID || got.From != from || got.To != to {
			t.Fatal("stream did not deliver exact asserted envelope")
		}
	case <-ctx.Done():
		t.Fatal("stream delivery deadline")
	}
	inbox, err := client.Inbox(ctx, to, messaging.Filter{})
	if err != nil || len(inbox) != 1 || inbox[0].ID != sent.ID {
		t.Fatal("Inbox", inbox, err)
	}
	if err := client.Consume(ctx, sent.ID, to); err != nil {
		t.Fatal("Consume", err)
	}
	stored, err := receiverDB.MessagingStore().Get(ctx, sent.ID)
	if err != nil || stored.ConsumedAt == nil {
		t.Fatal("recipient did not persist consumption", err)
	}

	// Scopes are exact, independent remote route permissions. Transport
	// authentication does not turn the asserted envelope addresses into actors.
	read := syntheticPeerDevice(t, receiver.Identity, []string{identity.ScopeRead})
	readPeer := peer
	readPeer.CredentialRef = syntheticPeerReference(t, read.Token)
	readStore, err := federation.HTTPDialer(nil)(readPeer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readStore.Inbox(ctx, to, messaging.Filter{}); err != nil {
		t.Fatal("read Inbox", err)
	}
	readCtx, stopRead := context.WithCancel(ctx)
	readStream, err := readStore.Subscribe(readCtx, to, messaging.Filter{})
	if err != nil {
		t.Fatal("read Subscribe", err)
	}
	stopRead()
	select {
	case _, ok := <-readStream:
		if ok {
			t.Fatal("unexpected read stream event")
		}
	case <-ctx.Done():
		t.Fatal("read cancellation")
	}
	if _, err := readStore.Send(ctx, messaging.Envelope{Kind: messaging.MsgKindNotice, From: from, To: to}); !errors.Is(err, federation.ErrPeerScope) {
		t.Fatal("read Send", err)
	}
	if err := readStore.Consume(ctx, sent.ID, to); !errors.Is(err, federation.ErrPeerScope) {
		t.Fatal("read Consume", err)
	}
	operate := syntheticPeerDevice(t, receiver.Identity, []string{identity.ScopeOperate})
	operatePeer := peer
	operatePeer.CredentialRef = syntheticPeerReference(t, operate.Token)
	operateStore, err := federation.HTTPDialer(nil)(operatePeer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := operateStore.Inbox(ctx, to, messaging.Filter{}); !errors.Is(err, federation.ErrPeerScope) {
		t.Fatal("operate Inbox", err)
	}
	if _, err := operateStore.Subscribe(ctx, to, messaging.Filter{}); !errors.Is(err, federation.ErrPeerScope) {
		t.Fatal("operate Subscribe", err)
	}
	opSent, err := operateStore.Send(ctx, messaging.Envelope{Kind: messaging.MsgKindNotice, From: from, To: to})
	if err != nil {
		t.Fatal("operate Send", err)
	}
	if err := operateStore.Consume(ctx, opSent.ID, to); err != nil {
		t.Fatal("operate Consume", err)
	}

	if err := receiver.Identity.RevokeDevice(ctx, device.Principal.ID); err != nil {
		t.Fatal(err)
	}
	// In-process device revocation closes both ends of the federated stream.
	for {
		select {
		case _, open := <-stream:
			if !open {
				goto revokedStreamClosed
			}
		case <-ctx.Done():
			t.Fatal("revoked federated stream stayed open")
		}
	}
revokedStreamClosed:
	if _, err := router.Send(ctx, messaging.Envelope{Kind: messaging.MsgKindNotice, From: from, To: to}); !errors.Is(err, federation.ErrPeerAuthentication) {
		t.Fatal("revoked token", err)
	}
	bad, err := identity.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	badPeer := peer
	badPeer.CredentialRef = syntheticPeerReference(t, bad)
	badStore, err := federation.HTTPDialer(nil)(badPeer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := badStore.Inbox(ctx, to, messaging.Filter{}); !errors.Is(err, federation.ErrPeerAuthentication) {
		t.Fatal("bad token", err)
	}
	// Expire only this synthetic principal in its private test database.
	if _, err := receiverDB.DB().ExecContext(ctx, "UPDATE principals SET expires_at=? WHERE principal_id=?", time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano), read.Principal.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := readStore.Inbox(ctx, to, messaging.Filter{}); !errors.Is(err, federation.ErrPeerAuthentication) {
		t.Fatal("expired token", err)
	}
	localTo := messaging.Address{Kind: messaging.KindAgent, Authority: "home", ID: "local"}
	local, err := client.Send(ctx, messaging.Envelope{Kind: messaging.MsgKindNotice, From: from, To: localTo})
	if err != nil {
		t.Fatal("bad remote credential broke local Send", err)
	}
	localInbox, err := client.Inbox(ctx, localTo, messaging.Filter{})
	if err != nil || len(localInbox) != 1 || localInbox[0].ID != local.ID {
		t.Fatal("local Inbox", err)
	}
	unknown := messaging.Address{Kind: messaging.KindAgent, Authority: "unknown", ID: "recipient"}
	if _, err := router.Send(ctx, messaging.Envelope{Kind: messaging.MsgKindNotice, From: from, To: unknown}); !errors.Is(err, federation.ErrNoRoute) {
		t.Fatal("strict routing changed", err)
	}
}
