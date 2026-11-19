package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/operatorattest"
	"github.com/hollis-labs/tether/internal/store"
)

// The operator attestation trust root (CW-20261114-0152,
// docs/operator-attested-replies.md). global.yaml's operator_attestation block
// holds Tangent's public keys. It is privileged: an operator-approved catalog
// plan records the block's digest as it applies it, and a catalog whose block
// has any other digest (a hand edit, any write path outside the privileged
// plan) is refused: at start the feature stays off, on reload the whole reload
// is refused and the previous keys stay in force. Both are audited.

// Audit events.
const (
	AuditOperatorAttestationKeysChanged = "operator_attestation_keys_changed"
	AuditOperatorAttestationRefused     = "operator_attestation_refused"
)

// OperatorAttestation is the trust root in force, nil when the feature is off.
func (s *Service) OperatorAttestation() *operatorattest.Config {
	return s.attestation.Load()
}

// checkOperatorAttestation returns the parsed block of cat when its digest is
// the approved one (nil config for an approved absence), or an error.
func (s *Service) checkOperatorAttestation(ctx context.Context, cat *config.Catalog) (*operatorattest.Config, error) {
	block := cat.Global.OperatorAttestation
	if s.Store == nil {
		// Nowhere to read an approval from: only an absent block is acceptable.
		if block != nil {
			return nil, fmt.Errorf("global.yaml operator_attestation cannot be checked against an approval without a state database")
		}
		return nil, nil
	}
	approved, ok, err := s.Store.ApprovedOperatorAttestation(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the approved operator attestation: %w", err)
	}
	want := ""
	if ok {
		want = approved.Digest
	}
	if got := operatorattest.Digest(block); got != want {
		return nil, fmt.Errorf("global.yaml operator_attestation is not the operator-approved trust root (apply it through `tether catalog plan` and an operator `tether catalog approve --ack-privileged`)")
	}
	if block == nil {
		return nil, nil
	}
	c, err := operatorattest.Parse(*block)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// loadOperatorAttestation installs the catalog's trust root at start; a block
// that is not the approved one leaves the feature off.
func (s *Service) loadOperatorAttestation(ctx context.Context, cat *config.Catalog) {
	if s.Store == nil || cat == nil {
		return
	}
	c, err := s.checkOperatorAttestation(ctx, cat)
	if err != nil {
		log.Printf("app: operator attestation off: %v", err)
		s.auditAttestation(ctx, AuditOperatorAttestationRefused, map[string]any{"phase": "start", "reason": err.Error()})
		s.attestation.Store(nil)
		return
	}
	s.attestation.Store(c)
}

// RecordOperatorAttestation is the catalog engine's OnOperatorAttestation:
// an operator-approved plan is applying next (nil: removing the block). It
// records next's digest as the approved trust root, audits the change, and
// returns how to put the previous record back if the apply rolls back.
func (s *Service) RecordOperatorAttestation(ctx context.Context, planID string, next *operatorattest.RawConfig) (func(), error) {
	ids := []string{}
	if next != nil {
		c, err := operatorattest.Parse(*next)
		if err != nil {
			return nil, err
		}
		ids = c.KeyIDs()
	}
	prev, had, err := s.Store.SetApprovedOperatorAttestation(ctx, store.OperatorAttestationTrust{Digest: operatorattest.Digest(next), KeyIDs: ids, PlanID: planID, ApprovedAt: time.Now().UTC()})
	if err != nil {
		return nil, err
	}
	s.auditAttestation(ctx, AuditOperatorAttestationKeysChanged, map[string]any{"plan_id": planID, "old_key_ids": prev.KeyIDs, "new_key_ids": ids})
	return func() {
		if err := s.Store.RestoreOperatorAttestation(context.WithoutCancel(ctx), prev, had); err != nil {
			log.Printf("app: restore the approved operator attestation after a rolled-back apply: %v", err)
		}
		s.auditAttestation(ctx, AuditOperatorAttestationKeysChanged, map[string]any{"plan_id": planID, "rolled_back": true, "old_key_ids": ids, "new_key_ids": prev.KeyIDs})
	}, nil
}

func (s *Service) auditAttestation(ctx context.Context, event string, detail map[string]any) {
	if s.Store == nil {
		log.Printf("app: %s %v (no state database to audit to)", event, detail)
		return
	}
	raw, _ := json.Marshal(detail)
	if err := identity.NewStore(s.Store.DB()).RecordObservation(context.WithoutCancel(ctx), identity.Observation{
		At: time.Now().UTC(), PrincipalID: identity.OperatorID, Mode: identity.Mode("catalog"), Authentication: "none",
		Method: "CATALOG", Route: "global.yaml#operator_attestation", Event: event, Detail: string(raw),
	}); err != nil {
		log.Printf("app: audit %s: %v", event, err)
	}
}
