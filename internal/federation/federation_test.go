package federation

import (
	"context"
	"errors"
	"testing"

	messaging "github.com/hollis-labs/go-messaging"
	"github.com/hollis-labs/go-messaging/memstore"
)

func TestBuildRouterDisabledReturnsNil(t *testing.T) {
	r, err := BuildRouter(Config{}, memstore.New(), nil)
	if err != nil {
		t.Fatalf("BuildRouter(disabled): %v", err)
	}
	if r != nil {
		t.Fatal("BuildRouter(disabled) should return a nil Router")
	}
}

func TestBuildRouterEnabledPeerless(t *testing.T) {
	cfg := Config{Enabled: true, LocalAuthority: "tether"}
	r, err := BuildRouter(cfg, memstore.New(), nil)
	if err != nil {
		t.Fatalf("BuildRouter(peerless): %v", err)
	}
	if r == nil {
		t.Fatal("BuildRouter(enabled) should return a Router")
	}
	if r.LocalAuthority() != "tether" || len(r.Authorities()) != 0 {
		t.Fatalf("peerless router misconfigured: authority=%q peers=%v",
			r.LocalAuthority(), r.Authorities())
	}
}

func TestBuildRouterRegistersPeers(t *testing.T) {
	cfg := Config{
		Enabled:        true,
		LocalAuthority: "tether",
		Peers: []Peer{
			{Authority: "torque", BaseURL: "http://torque.local"},
			{Authority: "nanite", BaseURL: "http://nanite.local"},
		},
	}
	// A dialer that hands back in-memory stores — no network needed.
	dial := func(p Peer) (messaging.Store, error) { return memstore.New(), nil }

	r, err := BuildRouter(cfg, memstore.New(), dial)
	if err != nil {
		t.Fatalf("BuildRouter: %v", err)
	}
	got := r.Authorities()
	if len(got) != 2 || got[0] != "nanite" || got[1] != "torque" {
		t.Fatalf("Authorities() = %v, want [nanite torque]", got)
	}
}

func TestBuildRouterStrictOption(t *testing.T) {
	cfg := Config{Enabled: true, LocalAuthority: "tether", Strict: true}
	r, err := BuildRouter(cfg, memstore.New(), nil)
	if err != nil {
		t.Fatalf("BuildRouter: %v", err)
	}
	_, err = r.Send(context.Background(), envTo(addr("elsewhere", "x")))
	if !errors.Is(err, ErrNoRoute) {
		t.Fatalf("strict router Send to unknown authority: got %v, want ErrNoRoute", err)
	}
}

func TestBuildRouterPeersRequireDialer(t *testing.T) {
	cfg := Config{
		Enabled:        true,
		LocalAuthority: "tether",
		Peers:          []Peer{{Authority: "torque", BaseURL: "http://torque.local"}},
	}
	if _, err := BuildRouter(cfg, memstore.New(), nil); err == nil {
		t.Fatal("BuildRouter with peers but no dialer should fail")
	}
}

func TestBuildRouterRejectsBadConfig(t *testing.T) {
	cfg := Config{Enabled: true} // missing local authority
	if _, err := BuildRouter(cfg, memstore.New(), nil); err == nil {
		t.Fatal("BuildRouter with an invalid config should fail")
	}
}

func TestBuildRouterRequiresLocalStore(t *testing.T) {
	cfg := Config{Enabled: true, LocalAuthority: "tether"}
	if _, err := BuildRouter(cfg, nil, nil); err == nil {
		t.Fatal("BuildRouter with a nil local store should fail")
	}
}
