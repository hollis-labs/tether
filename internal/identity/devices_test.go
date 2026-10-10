package identity_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/identity"
)

func TestDeviceRenewListRevokeAndAudit(t *testing.T) {
	s, db := identityStore(t)
	ctx := context.Background()
	grant, err := s.CreatePairingGrant(ctx, pairingOperator(), "synthetic worker", []string{"read", "operate"}, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	device, err := s.ExchangePairingGrant(ctx, grant.Code, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.Verify(ctx, device.Token)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RenewDevice(ctx, p, identity.HashToken(device.Token), []string{"admin"}); err == nil {
		t.Fatal("renew widened scopes")
	}
	if _, _, err := s.RenewDevice(ctx, p, strings.Repeat("0", 64), nil); err == nil {
		t.Fatal("renew ignored current credential")
	}
	if _, _, err := s.RenewDevice(ctx, p, identity.HashToken(device.Token), []string{"read"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RenewDevice(ctx, p, identity.HashToken(device.Token), nil); err == nil {
		t.Fatal("stale principal restored removed scope")
	}
	p, err = s.Verify(ctx, device.Token)
	if err != nil || len(p.Scopes) != 1 {
		t.Fatal("narrowing not persisted", err)
	}
	o := identity.Observation{At: time.Now(), Authentication: "verified", Method: "GET", Route: "/sessions"}
	if err := s.RecordDeviceUse(ctx, p, o, "192.0.2.7:7331", "synthetic-worker/1"); err != nil {
		t.Fatal(err)
	}
	devices, err := s.ListDevices(ctx)
	if err != nil || len(devices) != 1 {
		t.Fatal("device list failed", err)
	}
	if devices[0].ID != p.ID || devices[0].LastUsedAt == nil || devices[0].RemoteAddress != "192.0.2.7" || devices[0].UserAgent != "synthetic-worker/1" {
		t.Fatal("metadata not tied to device")
	}
	var audited string
	if err := db.DB().QueryRow(`SELECT principal_id FROM identity_audit WHERE route='/sessions'`).Scan(&audited); err != nil || audited != p.ID {
		t.Fatal("missing device use audit", err)
	}
	if err := s.RecordDeviceUse(ctx, p, o, "forged.invalid:1", device.Token); err != nil {
		t.Fatal(err)
	}
	devices, err = s.ListDevices(ctx)
	if err != nil || devices[0].UserAgent != "<redacted>" || devices[0].RemoteAddress != "" {
		t.Fatal("unsafe metadata retained", err)
	}
	if err := s.RevokeDevice(ctx, identity.OperatorID); !errors.Is(err, identity.ErrDeviceNotFound) {
		t.Fatal("device API revoked operator", err)
	}
	if err := s.RevokeDevice(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Verify(ctx, device.Token); !errors.Is(err, identity.ErrInvalidToken) {
		t.Fatal("revoked device still valid", err)
	}
	if _, _, err := s.RenewDevice(ctx, p, identity.HashToken(device.Token), nil); err == nil {
		t.Fatal("revoked renewal accepted")
	}
	devices, err = s.ListDevices(ctx)
	if err != nil || devices[0].RevokedAt == nil {
		t.Fatal("revocation missing from list", err)
	}
}
