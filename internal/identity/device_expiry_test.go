package identity_test

import (
	"context"
	"testing"
	"time"
)

func TestDeviceExpiryCountsBoundariesAndRevocations(t *testing.T) {
	s, db := identityStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	for i, delta := range []time.Duration{-time.Second, 0, time.Second, 7 * 24 * time.Hour, 7*24*time.Hour + time.Second, -time.Hour} {
		grant, err := s.CreatePairingGrant(ctx, pairingOperator(), "private label", []string{"read"}, 0, "")
		if err != nil {
			t.Fatal(err)
		}
		d, err := s.ExchangePairingGrant(ctx, grant.Code, nil, "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.DB().Exec(`UPDATE principals SET expires_at=? WHERE principal_id=?`, now.Add(delta).Format(time.RFC3339Nano), d.Principal.ID); err != nil {
			t.Fatal(err)
		}
		if i == 5 {
			if err := s.RevokeDevice(ctx, d.Principal.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	counts, err := s.DeviceExpiry(ctx, now)
	if err != nil || counts.Expiring != 2 || counts.Expired != 2 {
		t.Fatalf("counts=%+v err=%v", counts, err)
	}
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.DeviceExpiry(ctx, now); err == nil {
		t.Fatal("unavailable DB reported success")
	}
}
