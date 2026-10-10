package federation_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	messaging "github.com/hollis-labs/substrate/mesh/messaging"
	"github.com/hollis-labs/substrate/mesh/messaging/memstore"
	tether "github.com/hollis-labs/substrate/mesh/tetherclient"
	"github.com/hollis-labs/tether/internal/api"
	directory "github.com/hollis-labs/tether/internal/environmentdirectory"
	"github.com/hollis-labs/tether/internal/federation"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/messaging/wakeintent"
	"github.com/hollis-labs/tether/internal/store"
)

func TestDirectoryRoutingUsesCommittedTombstone(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	svc := directory.New(db, directory.Options{})
	now := time.Now().UTC()
	record := directory.Record{Registration: directory.Registration{EnvironmentTarget: tether.EnvironmentTarget{EnvironmentID: uuid.NewString(), Authority: "worker", Routes: []tether.EnvironmentRoute{{BaseURL: "http://127.0.0.1:1"}}, CredentialReference: "file:///synthetic"}, DeviceID: "device", Ownership: "external"}, State: "reachable", Protocol: 1, LastSeen: &now}
	if _, err = db.RegisterEnvironment(ctx, record); err != nil {
		t.Fatal(err)
	}
	resolver, err := federation.DirectoryAuthorityResolver(ctx, svc, federation.Config{}, tether.EnvironmentOptions{ResolveCredential: func(context.Context, string) (string, error) {
		t.Fatal("routing lookup must not read credentials")
		return "", errors.New("refused")
	}})
	if err != nil {
		t.Fatal(err)
	}
	local, static := memstore.New(), memstore.New()
	router := federation.NewRouter(local, "hub")
	router.SetAuthorityResolver(resolver)
	if err = router.Register("worker", static); err != nil {
		t.Fatal(err)
	}
	if router.IsLocal("worker") {
		t.Fatal("directory authority mistaken for local")
	}
	if _, owned, err := resolver(ctx, "worker"); err != nil || !owned {
		t.Fatal("lookup", owned, err)
	}
	if _, err = svc.Retire(ctx, record.EnvironmentID); err != nil {
		t.Fatal(err)
	}
	to := directoryAddr("worker", "agent")
	if _, err = router.Send(ctx, directoryEnvelope(to)); !errors.Is(err, federation.ErrNoRoute) {
		t.Fatal("retired Send", err)
	}
	if _, err = router.Inbox(ctx, to, messaging.Filter{}); !errors.Is(err, federation.ErrNoRoute) {
		t.Fatal("retired Inbox", err)
	}
	if _, err = router.Subscribe(ctx, to, messaging.Filter{}); !errors.Is(err, federation.ErrNoRoute) {
		t.Fatal("retired Subscribe", err)
	}
	if err = router.Consume(ctx, "id", to); !errors.Is(err, federation.ErrNoRoute) {
		t.Fatal("retired Consume", err)
	}
	for _, st := range []messaging.Store{local, static} {
		in, err := st.Inbox(ctx, to, messaging.Filter{})
		if err != nil || len(in) != 0 {
			t.Fatal("fallback delivery", err)
		}
	}
}

func TestDirectoryRefusesStaticBindingDrift(t *testing.T) {
	in := directory.Registration{EnvironmentTarget: tether.EnvironmentTarget{Authority: "worker", Routes: []tether.EnvironmentRoute{{BaseURL: "https://worker.example"}}, CredentialReference: "file:///device"}}
	cfg := federation.Config{Enabled: true, LocalAuthority: "hub", Peers: []federation.Peer{{Authority: "worker", BaseURL: "https://worker.example/", CredentialRef: "file:///device"}}}
	if err := federation.ValidateDirectoryBinding(in, cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Peers[0].CredentialRef = "file:///other"
	if err := federation.ValidateDirectoryBinding(in, cfg); !errors.Is(err, directory.ErrConflict) {
		t.Fatal("drift", err)
	}
	in.Authority = "hub"
	if err := federation.ValidateDirectoryBinding(in, cfg); !errors.Is(err, directory.ErrConflict) {
		t.Fatal("local authority", err)
	}
}

func directoryAddr(authority, id string) messaging.Address {
	return messaging.Address{Kind: messaging.KindAgent, Authority: authority, ID: id}
}
func directoryEnvelope(to messaging.Address) messaging.Envelope {
	return messaging.Envelope{Kind: messaging.MsgKindNotice, From: directoryAddr("hub", "sender"), To: to}
}

func TestDirectoryDeliveryUsesWorkerAdmission(t *testing.T) {
	ctx := context.Background()
	peer, err := store.Open(filepath.Join(t.TempDir(), "peer.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	hub, err := store.Open(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Close()
	id := uuid.NewString()
	var operate atomic.Bool
	var mutations atomic.Int32
	control := api.RemoteScopeMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth/context" {
			_, _ = w.Write([]byte(`{}`))
			return
		}
		api.NewHandler(api.Deps{MessageStore: peer.MessagingStore()}).ServeHTTP(w, r)
	}))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == tether.EnvironmentDescriptorPath {
			if r.Header.Get("Authorization") != "" {
				t.Error("credential on anonymous descriptor")
			}
			_ = json.NewEncoder(w).Encode(tether.EnvironmentDescriptor{EnvironmentID: id, Protocol: 1, ServerVersion: "synthetic"})
			return
		}
		if r.Method == http.MethodPost {
			mutations.Add(1)
		}
		if r.Header.Get("Authorization") != "Bearer synthetic-test" {
			w.WriteHeader(403)
			return
		}
		scopes := []string{identity.ScopeRead}
		if operate.Load() {
			scopes = append(scopes, identity.ScopeOperate)
		}
		principal := identity.Principal{ID: "synthetic-device", Kind: "device", Scopes: scopes}
		control.ServeHTTP(w, r.WithContext(identity.WithPrincipal(identity.WithRemoteContext(r.Context()), principal)))
	}))
	defer server.Close()
	opts := tether.EnvironmentOptions{ResolveCredential: func(context.Context, string) (string, error) { return "synthetic-test", nil }}
	svc := directory.New(hub, directory.Options{Client: opts})
	in := directory.Registration{EnvironmentTarget: tether.EnvironmentTarget{EnvironmentID: id, Authority: "worker", Routes: []tether.EnvironmentRoute{{BaseURL: server.URL}}, CredentialReference: "file:///synthetic"}, DeviceID: "synthetic-device", Ownership: "external", ManagementMode: directory.HubManaged}
	if _, err = svc.Register(ctx, in); err != nil {
		t.Fatal(err)
	}
	resolve, err := federation.DirectoryAuthorityResolver(ctx, svc, federation.Config{}, opts)
	if err != nil {
		t.Fatal(err)
	}
	router := federation.NewRouter(hub.MessagingStore(), "hub")
	router.SetAuthorityResolver(resolve)
	env := directoryEnvelope(directoryAddr("worker", "agent"))
	env.Metadata = map[string]string{wakeintent.OutcomeKey: "forged"}
	if _, err = router.Send(ctx, env); err == nil {
		t.Fatal("management metadata granted operate")
	}
	if mutations.Load() != 1 {
		t.Fatal("refused mutation replayed", mutations.Load())
	}
	operate.Store(true)
	sent, err := router.Send(ctx, env)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := peer.MessagingStore().Get(ctx, sent.ID)
	if err != nil {
		t.Fatal("peer missing delivery", err)
	}
	if saved.Metadata[wakeintent.OutcomeKey] != "" || saved.Metadata[wakeintent.MessageIDKey] == "" {
		t.Fatal("wake metadata invalid")
	}
	if env.Metadata[wakeintent.OutcomeKey] != "forged" {
		t.Fatal("mutated caller envelope")
	}
	if _, err = hub.MessagingStore().Get(ctx, sent.ID); !errors.Is(err, messaging.ErrNotFound) {
		t.Fatal("remote delivered locally", err)
	}
	if _, err = svc.Retire(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err = router.Send(ctx, env); !errors.Is(err, federation.ErrNoRoute) || mutations.Load() != 2 {
		t.Fatal("retired route called worker", err, mutations.Load())
	}
}
