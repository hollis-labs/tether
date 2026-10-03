package fabricenrollment

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hollis-labs/substrate/mesh"
	"github.com/hollis-labs/tether/internal/definitionresolve"
)

type authoredContent struct{ body []byte }

func (c *authoredContent) ReadDefinition(context.Context, string) ([]byte, error) {
	return append([]byte{}, c.body...), nil
}
func (*authoredContent) Pin(context.Context, string) (definitionresolve.ContentPin, error) {
	return definitionresolve.ContentPin{}, errors.New("no dependency fixture")
}

func TestEnrollmentAndDirectoryUseStrictVerifyingDefinitionStore(t *testing.T) {
	_, repo, _ := harness(t)
	content := &authoredContent{body: []byte(`---
schema_version: "2"
definition_id: integrated
revision: r1
name: integrated
description: Integration fixture
behavior:
  purpose: Review a synthetic fixture
capabilities:
  - id: review
    description: Review source
requirements: {}
harness_profile:
  context: {}
  permissions:
    profile: review
continuity:
  mode: durable
---
Private authored instructions.
`)}
	definitions, err := definitionresolve.NewDefinitionStore(repo, content, definitionresolve.Policy{KnownCapability: func(string) bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	indexed, err := definitions.Index(t.Context(), "urn:source:authored-fixture")
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(repo, definitions, func(_ context.Context, a Authorization) error {
		if a.Caller != a.Owner {
			return ErrDenied
		}
		return nil
	}, func(context.Context, Authorization, definitionresolve.VerifiedDefinition) (Publication, error) {
		return Publication{Publish: true, Capabilities: []string{"review"}}, nil
	}, func() time.Time { return now }, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	enrollment := Enrollment{Actor: mesh.Actor{URN: actorURN, Kind: mesh.ActorAgent}, Owner: owner, Definition: &indexed.Pin}
	if err := s.EnrollActor(t.Context(), owner, enrollment); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Directory(t.Context(), owner, owner, "", 100); err != nil {
		t.Fatal(err)
	}
	content.body = append(content.body, []byte("Changed behavior.\n")...)
	page, err := s.Directory(t.Context(), owner, owner, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range page.Records {
		if record.URN == actorURN {
			t.Fatal("directory published stale authored content")
		}
	}
	enrollment.Actor.URN = "msg://agent/example/changed-source"
	if err := s.EnrollActor(t.Context(), owner, enrollment); !errors.Is(err, definitionresolve.ErrPinMismatch) {
		t.Fatal("enrollment accepted stale authored content", err)
	}
	requireNotEnrolled(t, repo, enrollment.Actor.URN)
	content.body = []byte("invalid authored frontmatter")
	if _, err := s.Directory(t.Context(), owner, owner, "", 100); err != nil {
		t.Fatal("malformed row blocked page", err)
	}
	if err := s.EnrollActor(t.Context(), owner, enrollment); !errors.Is(err, definitionresolve.ErrContent) {
		t.Fatal("malformed content lost verification classification", err)
	}
}
