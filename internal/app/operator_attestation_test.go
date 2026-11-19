package app

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/operatorattest"
	"github.com/hollis-labs/tether/internal/store"
)

func attestationBlock(t *testing.T, ids ...string) (string, *operatorattest.RawConfig) {
	t.Helper()
	raw := &operatorattest.RawConfig{Issuer: "tangent", MaxAgeSeconds: 300}
	yaml := "\noperator_attestation:\n  issuer: tangent\n  max_age_seconds: 300\n  keys:\n"
	for _, id := range ids {
		pub, _, _ := ed25519.GenerateKey(rand.Reader)
		k := operatorattest.PublicKeyString(pub)
		raw.Keys = append(raw.Keys, operatorattest.RawKey{ID: id, PublicKey: k})
		yaml += "    - id: " + id + "\n      public_key: " + k + "\n"
	}
	return yaml, raw
}

func attestationService(t *testing.T) (*Service, string, string) {
	t.Helper()
	svc, root := reloadService(t)
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	svc.Store = db
	base, err := os.ReadFile(filepath.Join(root, "global.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return svc, root, string(base)
}

func auditEvents(t *testing.T, svc *Service, event string) int {
	t.Helper()
	var n int
	if err := svc.Store.DB().QueryRow(`SELECT count(*) FROM identity_audit WHERE event=?`, event).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// CW-20261114-0152: the trust root changes only through an operator-approved
// plan. A block nobody approved (a hand edit, any other write path) refuses
// the reload, keeps the previous keys, and is audited; the approved block is
// swapped in by the reload, no restart needed.
func TestOperatorAttestationReloadNeedsTheApprovedBlock(t *testing.T) {
	svc, root, base := attestationService(t)
	ctx := context.Background()
	block, raw := attestationBlock(t, "tangent-1")

	// Hand-added, never approved: refused.
	writeCat(t, root, "global.yaml", base+block)
	if _, err := svc.ReloadCatalog(ctx); err == nil || !strings.Contains(err.Error(), "operator-approved trust root") {
		t.Fatalf("unapproved block reloaded: %v", err)
	}
	if svc.OperatorAttestation() != nil || svc.CurrentCatalog().Global.OperatorAttestation != nil {
		t.Fatal("an unapproved trust root took effect")
	}
	if auditEvents(t, svc, AuditOperatorAttestationRefused) != 1 {
		t.Fatal("the refused reload was not audited")
	}

	// Approved (as an operator-approved plan's apply records it): takes effect live.
	if _, err := svc.RecordOperatorAttestation(ctx, "plan-1", raw); err != nil {
		t.Fatal(err)
	}
	if auditEvents(t, svc, AuditOperatorAttestationKeysChanged) != 1 {
		t.Fatal("the approval was not audited")
	}
	if _, err := svc.ReloadCatalog(ctx); err != nil {
		t.Fatalf("approved block refused: %v", err)
	}
	c := svc.OperatorAttestation()
	if c == nil || c.Issuer != "tangent" || len(c.Keys) != 1 || c.Keys[0].ID != "tangent-1" {
		t.Fatalf("trust root in force: %+v", c)
	}

	// A later hand edit (a second, unapproved key): refused; the approved keys stay in force.
	block2, _ := attestationBlock(t, "tangent-1", "rogue")
	writeCat(t, root, "global.yaml", base+block2)
	if _, err := svc.ReloadCatalog(ctx); err == nil {
		t.Fatal("an edited trust root reloaded")
	}
	if c = svc.OperatorAttestation(); c == nil || len(c.Keys) != 1 {
		t.Fatalf("after the refused edit: %+v", c)
	}
	// Removing the block by hand is refused too.
	writeCat(t, root, "global.yaml", base)
	if _, err := svc.ReloadCatalog(ctx); err == nil {
		t.Fatal("a hand-removed trust root reloaded")
	}
}

// At start, a block that is not the approved one leaves the feature off.
func TestOperatorAttestationStartsOffWithAnUnapprovedBlock(t *testing.T) {
	svc, root, base := attestationService(t)
	block, _ := attestationBlock(t, "tangent-1")
	writeCat(t, root, "global.yaml", base+block)
	cat := svc.CurrentCatalog()
	fresh := *cat
	fresh.Global.OperatorAttestation = &operatorattest.RawConfig{Issuer: "tangent", MaxAgeSeconds: 300, Keys: []operatorattest.RawKey{{ID: "x", PublicKey: "ed25519:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}}}
	svc.loadOperatorAttestation(context.Background(), &fresh)
	if svc.OperatorAttestation() != nil || auditEvents(t, svc, AuditOperatorAttestationRefused) != 1 {
		t.Fatal("an unapproved block at start turned the feature on")
	}
}
