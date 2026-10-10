package identity_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/store"
)

func pairingOperator() identity.Principal {
	return identity.Principal{ID: identity.OperatorID, Kind: "operator", Scopes: []string{"*"}}
}

func TestPairingAtomicConsumeAcrossConnections(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "pairing.db")
	first, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	second, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	ids := []*identity.Store{identity.NewStore(first.DB()), identity.NewStore(second.DB())}
	ctx := context.Background()
	grant, err := ids[0].CreatePairingGrant(ctx, pairingOperator(), "synthetic worker", []string{"read", "operate"}, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	var hash string
	if err := first.DB().QueryRow(`SELECT code_hash FROM pairing_grants WHERE id=?`, grant.ID).Scan(&hash); err != nil || hash != identity.HashToken(grant.Code) {
		t.Fatal("grant persisted incorrectly", err)
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make(chan identity.DeviceExchange, 16)
	failures := make(chan error, 16)
	for i := range 16 {
		wg.Go(func() {
			<-start
			result, err := ids[i%2].ExchangePairingGrant(ctx, grant.Code, []string{"read"}, "")
			if err == nil {
				results <- result
			} else {
				failures <- err
			}
		})
	}
	close(start)
	wg.Wait()
	close(results)
	close(failures)
	var winner identity.DeviceExchange
	successes := 0
	for result := range results {
		winner = result
		successes++
	}
	if successes != 1 {
		t.Fatalf("one-use grant produced %d credentials", successes)
	}
	for err := range failures {
		if !errors.Is(err, identity.ErrInvalidGrant) {
			t.Fatal("unexpected concurrent refusal", err)
		}
	}
	p, err := ids[1].Verify(ctx, winner.Token)
	if err != nil || p.Kind != "device" || len(p.Scopes) != 1 || p.Scopes[0] != "read" {
		t.Fatal("narrow device invalid", err)
	}
	if p.ExpiresAt == nil || time.Until(*p.ExpiresAt) < identity.DeviceLifetime-time.Minute {
		t.Fatal("device default expiry missing")
	}
	if err := first.DB().QueryRow(`SELECT token_hash FROM principals WHERE principal_id=?`, p.ID).Scan(&hash); err != nil || hash != identity.HashToken(winner.Token) {
		t.Fatal("device persisted incorrectly", err)
	}
}

func TestPairingRefusalsAndRollback(t *testing.T) {
	s, db := identityStore(t)
	ctx := context.Background()
	create := func() identity.IssuedGrant {
		t.Helper()
		g, err := s.CreatePairingGrant(ctx, pairingOperator(), "fixture", []string{"read"}, 0, "bound-key")
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	grant := create()
	for _, request := range []struct {
		code   string
		scopes []string
		thumb  string
	}{{grant.Code, []string{"operate"}, "bound-key"}, {grant.Code, nil, "wrong-key"}, {"tpg_bad", nil, "bound-key"}, {grant.Code, []string{}, "bound-key"}} {
		if _, err := s.ExchangePairingGrant(ctx, request.code, request.scopes, request.thumb); !errors.Is(err, identity.ErrInvalidGrant) {
			t.Fatal("invalid grant accepted", err)
		}
	}
	if _, err := db.DB().Exec(`CREATE TRIGGER fail_device_mint BEFORE INSERT ON principals WHEN NEW.kind='device' BEGIN SELECT RAISE(ABORT,'synthetic failure'); END;`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ExchangePairingGrant(ctx, grant.Code, nil, "bound-key"); err == nil {
		t.Fatal("failed mint accepted")
	}
	if _, err := db.DB().Exec(`DROP TRIGGER fail_device_mint`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ExchangePairingGrant(ctx, grant.Code, nil, "bound-key"); err != nil {
		t.Fatal("failed mint consumed grant", err)
	}
	if _, err := s.ExchangePairingGrant(ctx, grant.Code, nil, "bound-key"); !errors.Is(err, identity.ErrInvalidGrant) {
		t.Fatal("replay accepted", err)
	}
	for _, state := range []string{"expired", "revoked"} {
		grant := create()
		if state == "revoked" {
			if err := s.RevokePairingGrant(ctx, grant.ID); err != nil {
				t.Fatal(err)
			}
		} else {
			if _, err := db.DB().Exec(`UPDATE pairing_grants SET expires_at='2000-01-01T00:00:00.000000000Z' WHERE id=?`, grant.ID); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := s.ExchangePairingGrant(ctx, grant.Code, nil, "bound-key"); !errors.Is(err, identity.ErrInvalidGrant) {
			t.Fatal("inactive grant accepted", err)
		}
	}
	if _, err := s.CreatePairingGrant(ctx, identity.Principal{ID: "service", Kind: "service"}, "", []string{"read"}, 0, ""); err == nil {
		t.Fatal("nonoperator created grant")
	}
}
