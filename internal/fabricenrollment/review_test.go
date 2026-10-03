package fabricenrollment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"testing"

	"github.com/hollis-labs/substrate/mesh"
	"github.com/hollis-labs/tether/internal/definitionresolve"
	"github.com/hollis-labs/tether/internal/fabricstore"
)

func TestDirectoryOmitsInvalidPinWithoutBlockingHealthyAgent(t *testing.T) {
	for _, invalid := range []error{definitionresolve.ErrPinMismatch, definitionresolve.ErrContent, fmt.Errorf("malformed: %w", definitionresolve.ErrContent)} {
		t.Run(invalid.Error(), func(t *testing.T) {
			s, _, defs := harness(t)
			enroll(t, s, actorURN)
			healthyURN := mesh.URN("msg://agent/example/healthy")
			healthy := request(healthyURN)
			healthy.Definition = &nextPin
			if err := s.EnrollActor(t.Context(), owner, healthy); err != nil {
				t.Fatal(err)
			}
			s.definitions = definitionsFunc(func(ctx context.Context, p mesh.DefinitionRef) (definitionresolve.VerifiedDefinition, error) {
				if p == pin {
					return definitionresolve.VerifiedDefinition{}, invalid
				}
				return defs.Load(ctx, p)
			})
			page, err := s.Directory(t.Context(), owner, owner, "", 100)
			if err != nil {
				t.Fatal(err)
			}
			foundHealthy := false
			for _, record := range page.Records {
				if record.URN == actorURN {
					t.Fatal("published invalid pin")
				}
				if record.URN == healthyURN {
					foundHealthy = true
					if record.Definition == nil || *record.Definition != nextPin {
						t.Fatal(record)
					}
				}
			}
			if !foundHealthy {
				t.Fatal("one invalid pin blocked healthy directory row")
			}
		})
	}
}
func TestDirectoryPreservesOperationalErrors(t *testing.T) {
	unknown := errors.New("unexpected verifier fault")
	faults := []error{io.ErrUnexpectedEOF, context.Canceled, context.DeadlineExceeded, fabricstore.ErrNotFound, fabricstore.ErrConflict, unknown, &fs.PathError{Op: "read", Path: "fixture", Err: fs.ErrPermission}, errors.Join(definitionresolve.ErrContent, io.ErrUnexpectedEOF), errors.Join(definitionresolve.ErrContent, fabricstore.ErrInvalid)}
	for _, fault := range faults {
		t.Run(fault.Error(), func(t *testing.T) {
			s, _, _ := harness(t)
			enroll(t, s, actorURN)
			s.definitions = definitionsFunc(func(context.Context, mesh.DefinitionRef) (definitionresolve.VerifiedDefinition, error) {
				return definitionresolve.VerifiedDefinition{}, fault
			})
			if _, err := s.Directory(t.Context(), owner, owner, "", 100); !errors.Is(err, fault) {
				t.Fatal("operational failure was omitted", err)
			}
		})
	}
}
func TestAbsentLifecycleTargetIsDeniedLikeUnauthorizedExistingTarget(t *testing.T) {
	s, _, defs := harness(t)
	enroll(t, s, actorURN)
	stranger := mesh.URN("msg://user/example/stranger")
	for _, urn := range []mesh.URN{actorURN, "msg://agent/example/missing"} {
		if err := s.RebindAgent(t.Context(), stranger, urn, nextPin, 1); !errors.Is(err, ErrDenied) {
			t.Fatal("rebind exposed existence", urn, err)
		}
		if err := s.RetireActor(t.Context(), stranger, urn, 1); !errors.Is(err, ErrDenied) {
			t.Fatal("retirement exposed existence", urn, err)
		}
	}
	if defs.reads != 1 {
		t.Fatal("denied lifecycle mutation read definition")
	}
}
func TestImportDigestErrorsAndInvalidUTF8Refuse(t *testing.T) {
	_, err := digest("fixture", func() {})
	var cause *json.UnsupportedTypeError
	if !errors.Is(err, ErrManifest) || !errors.As(err, &cause) {
		t.Fatal("marshal error swallowed", err)
	}
	s, repo, _ := harness(t)
	cases := []struct {
		name  string
		alter func(*SourceSnapshot, *[]Mapping)
	}{
		{"source", func(s *SourceSnapshot, _ *[]Mapping) { s.Source = "invalid\xff" }},
		{"key", func(s *SourceSnapshot, m *[]Mapping) {
			s.Identities[0].Key = "invalid\xff"
			(*m)[0].Key = s.Identities[0].Key
		}},
		{"URN", func(s *SourceSnapshot, m *[]Mapping) {
			s.Identities[0].Actor.URN = "msg://agent/example/invalid\xff"
			(*m)[0].ActorURN = s.Identities[0].Actor.URN
		}},
		{"owner", func(s *SourceSnapshot, _ *[]Mapping) { s.Identities[0].Owner = "msg://user/example/invalid\xff" }},
		{"pin", func(_ *SourceSnapshot, m *[]Mapping) { p := pin; p.ID = "invalid\xff"; (*m)[0].Definition = &p }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src, maps := manifest()
			tc.alter(&src, &maps)
			if _, err := s.ApplyImport(t.Context(), owner, src, maps, "urn:approval:fixture"); !errors.Is(err, ErrManifest) {
				t.Fatal("JSON replacement accepted", err)
			}
		})
	}
	bad := request("msg://agent/example/invalid\xff")
	if err := s.EnrollActor(t.Context(), owner, bad); !errors.Is(err, fabricstore.ErrInvalid) {
		t.Fatal(err)
	}
	requireNotEnrolled(t, repo, actorURN)
}
