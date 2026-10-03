package fabricenrollment

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"unicode/utf8"

	"github.com/hollis-labs/substrate/mesh"
	"github.com/hollis-labs/tether/internal/fabricstore"
)

// SourceSnapshot contains only identity references exported by an authorized
// host. It is not a catalog reader, content export or authoritative live view.
type SourceSnapshot struct {
	Source     string           `json:"source"`
	Identities []SourceIdentity `json:"identities"`
}
type SourceIdentity struct {
	Key   string     `json:"key"`
	Actor mesh.Actor `json:"actor"`
	Owner mesh.URN   `json:"owner"`
}

// Mapping is an explicit, reviewed apply-time input. No name/profile matching
// or inferred pin is permitted. ActorURN must equal the source bytes exactly.
type Mapping struct {
	Key        string              `json:"key"`
	ActorURN   mesh.URN            `json:"actor_urn"`
	Definition *mesh.DefinitionRef `json:"definition,omitempty"`
}
type ImportProblem struct{ Key, Reason string }
type ImportPreview struct {
	SourceSnapshotDigest string
	MappingDigest        string
	Ready                bool
	AlreadyApplied       bool
	Problems             []ImportProblem
	Receipts             []fabricstore.ImportReceipt
}
type importPlan struct {
	source      SourceSnapshot
	mappings    []Mapping
	preview     ImportPreview
	enrollments []Enrollment
	receiptIDs  []string
}

const maxImportTextBytes = 4096

func digest(version string, value any) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrManifest, err)
	}
	sum := sha256.Sum256(append([]byte(version+"\n"), raw...))
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
func prepare(source SourceSnapshot, mappings []Mapping) (importPlan, error) {
	plan := importPlan{source: SourceSnapshot{Source: source.Source, Identities: append([]SourceIdentity{}, source.Identities...)}, mappings: append([]Mapping{}, mappings...)}
	if source.Source == "" || len(source.Source) > maxImportTextBytes || !utf8.ValidString(source.Source) || len(source.Identities) == 0 || len(source.Identities) > 100 || len(mappings) > 100 {
		return plan, ErrManifest
	}
	for i := range plan.mappings {
		if plan.mappings[i].Definition != nil {
			pin := *plan.mappings[i].Definition
			plan.mappings[i].Definition = &pin
		}
	}
	sort.Slice(plan.source.Identities, func(i, j int) bool { return plan.source.Identities[i].Key < plan.source.Identities[j].Key })
	sort.Slice(plan.mappings, func(i, j int) bool { return plan.mappings[i].Key < plan.mappings[j].Key })
	byKey := map[string]Mapping{}
	sourceKeys := map[string]bool{}
	urns := map[mesh.URN]bool{}
	for _, identity := range plan.source.Identities {
		if identity.Key == "" || len(identity.Key) > maxImportTextBytes || !utf8.ValidString(identity.Key) || !utf8.ValidString(string(identity.Actor.URN)) || !utf8.ValidString(string(identity.Owner)) || sourceKeys[identity.Key] || urns[identity.Actor.URN] || identity.Actor.Validate() != nil || identity.Owner.Validate() != nil {
			return plan, ErrManifest
		}
		sourceKeys[identity.Key] = true
		urns[identity.Actor.URN] = true
	}
	for _, mapping := range plan.mappings {
		if _, duplicate := byKey[mapping.Key]; duplicate || !utf8.ValidString(mapping.Key) || !sourceKeys[mapping.Key] {
			return plan, ErrManifest
		}
		byKey[mapping.Key] = mapping
	}
	var err error
	plan.preview.SourceSnapshotDigest, err = digest("fabric-import-source-v1", plan.source)
	if err != nil {
		return plan, err
	}
	plan.preview.MappingDigest, err = digest("fabric-import-mapping-v1", plan.mappings)
	if err != nil {
		return plan, err
	}
	for _, identity := range plan.source.Identities {
		mapping, found := byKey[identity.Key]
		if !found {
			plan.preview.Problems = append(plan.preview.Problems, ImportProblem{identity.Key, "missing explicit mapping"})
			continue
		}
		if mapping.ActorURN != identity.Actor.URN {
			return plan, ErrManifest
		}
		enrollment := Enrollment{Actor: identity.Actor, Owner: identity.Owner, Definition: mapping.Definition}
		if validateEnrollment(enrollment) != nil {
			return plan, ErrManifest
		}
		plan.enrollments = append(plan.enrollments, enrollment)
		id, err := digest("fabric-import-receipt-v1", []string{plan.preview.SourceSnapshotDigest, plan.preview.MappingDigest, identity.Key})
		if err != nil {
			return plan, err
		}
		plan.receiptIDs = append(plan.receiptIDs, id)
	}
	return plan, nil
}
func (s *Service) authorizeImport(ctx context.Context, caller mesh.URN, plan importPlan) error {
	for _, identity := range plan.source.Identities {
		var pin *mesh.DefinitionRef
		for _, mapping := range plan.mappings {
			if mapping.Key == identity.Key {
				pin = mapping.Definition
				break
			}
		}
		if err := s.allow(ctx, Authorization{Caller: caller, Owner: identity.Owner, Target: identity.Actor.URN, Action: Import, Definition: pin, Source: plan.source.Source}); err != nil {
			return err
		}
	}
	return nil
}
func sameReceipt(receipt fabricstore.ImportReceipt, plan importPlan, i int) bool {
	return receipt.ID == plan.receiptIDs[i] && receipt.CandidateID == receipt.ID && receipt.ActorURN == plan.enrollments[i].Actor.URN && receipt.SourceSnapshotDigest == plan.preview.SourceSnapshotDigest && receipt.MappingDigest == plan.preview.MappingDigest
}

// PreviewImport never writes, including for unresolved candidates. Missing
// mappings are reported; ambiguity, altered URNs and identity merges refuse.
func (s *Service) PreviewImport(ctx context.Context, caller mesh.URN, source SourceSnapshot, mappings []Mapping) (ImportPreview, error) {
	plan, err := prepare(source, mappings)
	if err != nil {
		return ImportPreview{}, err
	}
	if err := s.authorizeImport(ctx, caller, plan); err != nil {
		return ImportPreview{}, err
	}
	if len(plan.preview.Problems) != 0 {
		return plan.preview, nil
	}
	prior, err := s.priorReceipts(ctx, plan)
	if err != nil {
		return ImportPreview{}, err
	}
	if len(prior) > 0 {
		plan.preview.Receipts = prior
		plan.preview.Ready = true
		plan.preview.AlreadyApplied = true
		return plan.preview, nil
	}
	for i, enrollment := range plan.enrollments {
		if err := s.verify(ctx, enrollment); err != nil {
			return ImportPreview{}, err
		}
		_, err := s.repo.Actor(ctx, enrollment.Actor.URN)
		if err == nil {
			plan.preview.Problems = append(plan.preview.Problems, ImportProblem{plan.source.Identities[i].Key, "identity already enrolled; merge refused"})
		} else if !errors.Is(err, fabricstore.ErrNotFound) {
			return ImportPreview{}, err
		}
		_, err = s.repo.LegacyRef(ctx, source.Source, plan.source.Identities[i].Key)
		if err == nil {
			plan.preview.Problems = append(plan.preview.Problems, ImportProblem{plan.source.Identities[i].Key, "source identity already imported"})
		} else if !errors.Is(err, fabricstore.ErrNotFound) {
			return ImportPreview{}, err
		}
	}
	plan.preview.Ready = len(plan.preview.Problems) == 0
	return plan.preview, nil
}

func (s *Service) priorReceipts(ctx context.Context, plan importPlan) ([]fabricstore.ImportReceipt, error) {
	records, err := s.repo.Receipts(ctx, plan.receiptIDs)
	if err != nil {
		return nil, err
	}
	if len(records) == 0 {
		return nil, nil
	}
	if len(records) != len(plan.receiptIDs) {
		return nil, fabricstore.ErrConflict
	}
	result := []fabricstore.ImportReceipt{}
	for i, record := range records {
		if !sameReceipt(record.Value, plan, i) {
			return nil, fabricstore.ErrConflict
		}
		result = append(result, record.Value)
	}
	return result, nil
}

// ApplyImport commits all identities and receipts together. Retry identity is
// the computed source snapshot plus mapping digest, independent of input order.
// ApprovalRef is audit provenance; host authorization is still required on retry.
// This kernel has no live source adapter and never creates guessed pins.
func (s *Service) ApplyImport(ctx context.Context, caller mesh.URN, source SourceSnapshot, mappings []Mapping, approvalRef string) ([]fabricstore.ImportReceipt, error) {
	plan, err := prepare(source, mappings)
	if err != nil {
		return nil, err
	}
	if approvalRef == "" || !utf8.ValidString(approvalRef) || len(approvalRef) > maxImportTextBytes || len(plan.preview.Problems) != 0 {
		return nil, ErrManifest
	}
	if err := s.authorizeImport(ctx, caller, plan); err != nil {
		return nil, err
	}
	prior, err := s.priorReceipts(ctx, plan)
	if err != nil {
		return nil, err
	}
	if len(prior) > 0 {
		return prior, nil
	}
	for _, enrollment := range plan.enrollments {
		if err := s.verify(ctx, enrollment); err != nil {
			return nil, err
		}
	}
	at := s.now().UTC()
	if at.IsZero() {
		return nil, fabricstore.ErrInvalid
	}
	receipts := []fabricstore.ImportReceipt{}
	err = s.repo.Write(ctx, func(tx *fabricstore.Tx) error {
		prior := []fabricstore.ImportReceipt{}
		for i, id := range plan.receiptIDs {
			receipt, err := tx.Receipt(id)
			if errors.Is(err, fabricstore.ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			if !sameReceipt(receipt.Value, plan, i) {
				return fabricstore.ErrConflict
			}
			prior = append(prior, receipt.Value)
		}
		if len(prior) > 0 {
			if len(prior) != len(plan.receiptIDs) {
				return fabricstore.ErrConflict
			}
			receipts = prior
			return nil
		}
		// All pins were verified before reserving SQLite's writer. Creation and
		// receipt constraints now refuse races with other enroll/import operations.
		for i, enrollment := range plan.enrollments {
			id := plan.receiptIDs[i]
			if err := create(tx, enrollment); err != nil {
				return err
			}
			if err := s.event(tx, enrollment, Enroll, 1, mesh.EnrollmentActive); err != nil {
				return err
			}
			candidate := fabricstore.MigrationCandidate{ID: id, LegacyRef: plan.source.Source + "/" + plan.source.Identities[i].Key, EvidenceRef: plan.preview.SourceSnapshotDigest, Reason: "explicit reviewed mapping", State: "reviewed"}
			if err := tx.PutCandidate(candidate, 0); err != nil {
				return err
			}
			receipt := fabricstore.ImportReceipt{ID: id, CandidateID: id, ActorURN: enrollment.Actor.URN, ApprovalRef: approvalRef, At: at, SourceSnapshotDigest: plan.preview.SourceSnapshotDigest, MappingDigest: plan.preview.MappingDigest}
			if err := tx.AddReceipt(receipt); err != nil {
				return err
			}
			if err := tx.AddLegacyRef(fabricstore.LegacyRef{Source: plan.source.Source, Key: plan.source.Identities[i].Key, ActorURN: enrollment.Actor.URN, ReceiptID: id}); err != nil {
				return err
			}
			receipts = append(receipts, receipt)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return receipts, nil
}
