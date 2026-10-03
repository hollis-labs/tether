package fabricenrollment

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/hollis-labs/substrate/mesh"
	"github.com/hollis-labs/tether/internal/definitionresolve"
	"github.com/hollis-labs/tether/internal/fabricstore"
)

func manifest() (SourceSnapshot, []Mapping) {
	second := mesh.URN("msg://agent/example/Second-Identity")
	return SourceSnapshot{Source: "urn:source:synthetic-registry", Identities: []SourceIdentity{{Key: "first", Actor: mesh.Actor{URN: actorURN, Kind: mesh.ActorAgent}, Owner: owner}, {Key: "second", Actor: mesh.Actor{URN: second, Kind: mesh.ActorAgent}, Owner: owner}}}, []Mapping{{Key: "first", ActorURN: actorURN, Definition: &pin}, {Key: "second", ActorURN: second, Definition: &nextPin}}
}
func TestImportPreviewReadOnlyApplyAtomicAndRetryOrderIndependent(t *testing.T) {
	s, r, defs := harness(t)
	source, mappings := manifest()
	preview, err := s.PreviewImport(t.Context(), owner, source, mappings)
	if err != nil || !preview.Ready || preview.AlreadyApplied || preview.SourceSnapshotDigest == "" || preview.MappingDigest == "" {
		t.Fatal(preview, err)
	}
	for _, identity := range source.Identities {
		requireNotEnrolled(t, r, identity.Actor.URN)
	}
	receipts, err := s.ApplyImport(t.Context(), owner, source, mappings, "urn:approval:reviewed")
	if err != nil {
		t.Fatal(err)
	}
	byURN := map[mesh.URN]fabricstore.ImportReceipt{}
	for _, receipt := range receipts {
		byURN[receipt.ActorURN] = receipt
		if receipt.SourceSnapshotDigest != preview.SourceSnapshotDigest || receipt.MappingDigest != preview.MappingDigest || receipt.ApprovalRef != "urn:approval:reviewed" {
			t.Fatal("wrong provenance", receipt)
		}
	}
	for _, identity := range source.Identities {
		receipt, found := byURN[identity.Actor.URN]
		if !found {
			t.Fatal("missing identity receipt", identity.Actor.URN)
		}
		got, err := r.Actor(t.Context(), identity.Actor.URN)
		if err != nil || got.Value.URN != identity.Actor.URN || got.Value.Owner != identity.Owner {
			t.Fatal(got, err)
		}
		link, err := r.LegacyRef(t.Context(), source.Source, identity.Key)
		if err != nil || link.Value.ActorURN != identity.Actor.URN || link.Value.ReceiptID != receipt.ID {
			t.Fatal(link, err)
		}
	}
	// Reorder the same source and mapping, retire one actor and remove content.
	// A lost-response retry returns the committed receipts, not a new enrollment.
	source.Identities[0], source.Identities[1] = source.Identities[1], source.Identities[0]
	mappings[0], mappings[1] = mappings[1], mappings[0]
	if err := s.RetireActor(t.Context(), owner, actorURN, 1); err != nil {
		t.Fatal(err)
	}
	defs.values = map[mesh.DefinitionRef]definitionresolve.VerifiedDefinition{}
	repeated, err := s.ApplyImport(t.Context(), owner, source, mappings, "urn:approval:retry")
	if err != nil || !reflect.DeepEqual(receipts, repeated) {
		t.Fatal("idempotency changed receipts", repeated, err)
	}
	again, err := s.PreviewImport(t.Context(), owner, source, mappings)
	if err != nil || !again.AlreadyApplied {
		t.Fatal(again, err)
	}
	actor, err := r.Actor(t.Context(), actorURN)
	if err != nil || actor.Value.Lifecycle != mesh.EnrollmentRetired {
		t.Fatal("retry reactivated identity", actor, err)
	}
	if _, err := s.ApplyImport(t.Context(), "msg://user/example/stranger", source, mappings, "urn:approval:retry"); !errors.Is(err, ErrDenied) {
		t.Fatal("retry bypassed authority", err)
	}
}
func TestImportMissingMappingRemainsUnresolvedAndAmbiguityRefuses(t *testing.T) {
	s, r, _ := harness(t)
	source, mappings := manifest()
	preview, err := s.PreviewImport(t.Context(), owner, source, mappings[:1])
	if err != nil || preview.Ready {
		t.Fatal(preview, err)
	}
	found := false
	for _, problem := range preview.Problems {
		if problem.Key == "second" {
			found = true
		}
	}
	if !found {
		t.Fatal("missing mapping not reported")
	}
	if _, err := s.ApplyImport(t.Context(), owner, source, mappings[:1], "urn:approval:fixture"); !errors.Is(err, ErrManifest) {
		t.Fatal(err)
	}
	for _, identity := range source.Identities {
		requireNotEnrolled(t, r, identity.Actor.URN)
	}
	cases := []struct {
		name   string
		change func(*SourceSnapshot, *[]Mapping)
	}{
		{"duplicate source key", func(s *SourceSnapshot, _ *[]Mapping) { s.Identities[1].Key = s.Identities[0].Key }},
		{"duplicate original identity", func(s *SourceSnapshot, m *[]Mapping) {
			s.Identities[1].Actor = s.Identities[0].Actor
			(*m)[1].ActorURN = s.Identities[0].Actor.URN
		}},
		{"multiple mappings", func(_ *SourceSnapshot, m *[]Mapping) { *m = append(*m, (*m)[0]) }},
		{"extra mapping", func(_ *SourceSnapshot, m *[]Mapping) {
			*m = append(*m, Mapping{Key: "unknown", ActorURN: actorURN, Definition: &pin})
		}},
		{"URN changed in case", func(_ *SourceSnapshot, m *[]Mapping) { (*m)[0].ActorURN = "msg://agent/example/original-identity" }},
		{"kind mismatch", func(s *SourceSnapshot, _ *[]Mapping) { s.Identities[0].Actor.Kind = mesh.ActorService }},
		{"missing agent pin", func(_ *SourceSnapshot, m *[]Mapping) { (*m)[0].Definition = nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src, m := manifest()
			tc.change(&src, &m)
			if _, err := s.PreviewImport(t.Context(), owner, src, m); !errors.Is(err, ErrManifest) {
				t.Fatal("ambiguous import accepted", err)
			}
		})
	}
}
func TestImportChangedMappingOrSnapshotDoesNotMergeExistingIdentity(t *testing.T) {
	s, r, _ := harness(t)
	source, mappings := manifest()
	if _, err := s.ApplyImport(t.Context(), owner, source, mappings, "urn:approval:fixture"); err != nil {
		t.Fatal(err)
	}
	mappings[0].Definition = &nextPin
	if _, err := s.ApplyImport(t.Context(), owner, source, mappings, "urn:approval:fixture"); !errors.Is(err, fabricstore.ErrConflict) {
		t.Fatal("mapping change overwrote imported identity", err)
	}
	current, err := r.Agent(t.Context(), actorURN)
	if err != nil || current.Value.Definition != pin {
		t.Fatal(current, err)
	}
	source.Source = "urn:source:different-snapshot"
	if _, err := s.ApplyImport(t.Context(), owner, source, mappings, "urn:approval:fixture"); !errors.Is(err, fabricstore.ErrConflict) {
		t.Fatal("snapshot change merged identity", err)
	}
}
func TestImportTransactionFailureRollsBackEarlierIdentityAndReceipt(t *testing.T) {
	s, r, _ := harness(t)
	source, mappings := manifest()
	late := source.Identities[1].Actor.URN
	// A colliding outbox event makes the second identity fail after the first
	// enrollment, candidate, receipt and provenance link were written.
	if err := r.Write(t.Context(), func(tx *fabricstore.Tx) error { return s.event(tx, request(late), Enroll, 1, mesh.EnrollmentActive) }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyImport(t.Context(), owner, source, mappings, "urn:approval:fixture"); !errors.Is(err, fabricstore.ErrConflict) {
		t.Fatal(err)
	}
	requireNotEnrolled(t, r, actorURN)
	requireNotEnrolled(t, r, late)
	if _, err := r.LegacyRef(t.Context(), source.Source, "first"); !errors.Is(err, fabricstore.ErrNotFound) {
		t.Fatal("provenance escaped failed apply", err)
	}
}
func TestImportNonAgentParticipantsAndAuthorizationBeforeContent(t *testing.T) {
	s, r, defs := harness(t)
	src := SourceSnapshot{Source: "urn:source:participants", Identities: []SourceIdentity{{Key: "person", Actor: mesh.Actor{URN: owner, Kind: mesh.ActorUser}, Owner: owner}}}
	maps := []Mapping{{Key: "person", ActorURN: owner}}
	if _, err := s.ApplyImport(t.Context(), owner, src, maps, "urn:approval:participants"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Agent(t.Context(), owner); !errors.Is(err, fabricstore.ErrNotFound) {
		t.Fatal("human acquired agent record", err)
	}
	source, mappings := manifest()
	source.Identities[1].Owner = "msg://user/example/other-owner"
	before := defs.reads
	if _, err := s.ApplyImport(t.Context(), owner, source, mappings, "urn:approval:fixture"); !errors.Is(err, ErrDenied) {
		t.Fatal(err)
	}
	if defs.reads != before {
		t.Fatal("partially authorized import read content")
	}
	requireNotEnrolled(t, r, actorURN)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.PreviewImport(ctx, owner, src, maps); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

type definitionsFunc func(context.Context, mesh.DefinitionRef) (definitionresolve.VerifiedDefinition, error)

func (f definitionsFunc) Load(ctx context.Context, p mesh.DefinitionRef) (definitionresolve.VerifiedDefinition, error) {
	return f(ctx, p)
}

func TestConcurrentImportRetryReturnsSameCommittedReceipts(t *testing.T) {
	s, _, defs := harness(t)
	source, mappings := manifest()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	s.definitions = definitionsFunc(func(ctx context.Context, p mesh.DefinitionRef) (definitionresolve.VerifiedDefinition, error) {
		if p == pin {
			entered <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
				return definitionresolve.VerifiedDefinition{}, ctx.Err()
			}
		}
		return defs.Load(ctx, p)
	})
	type outcome struct {
		receipts []fabricstore.ImportReceipt
		err      error
	}
	results := make(chan outcome, 2)
	for range 2 {
		go func() {
			receipts, err := s.ApplyImport(ctx, owner, source, mappings, "urn:approval:concurrent")
			results <- outcome{receipts, err}
		}()
	}
	for range 2 {
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("concurrent import did not reach verification", ctx.Err())
		}
	}
	close(release)
	first, second := <-results, <-results
	if first.err != nil || second.err != nil || !reflect.DeepEqual(first.receipts, second.receipts) {
		t.Fatal("concurrent retry split operation", first, second)
	}
}
