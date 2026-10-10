package fabricenrollment

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/hollis-labs/substrate/mesh"
	"github.com/hollis-labs/tether/internal/fabricstore"
)

func TestEnrollmentAuthorityStoredShownAndPreservedWithoutGrant(t *testing.T) {
	s, repo, _ := harness(t)
	r := request(actorURN)
	r.Authority = fabricstore.AuthorityHub
	other := mesh.URN("msg://user/example/other")
	if err := s.EnrollActor(t.Context(), other, r); !errors.Is(err, ErrDenied) {
		t.Fatal("hub label granted authority", err)
	}
	if err := s.EnrollActor(t.Context(), owner, r); err != nil {
		t.Fatal(err)
	}
	actor, err := repo.Actor(t.Context(), actorURN)
	if err != nil || actor.Value.Authority != fabricstore.AuthorityHub {
		t.Fatal(actor, err)
	}
	page, err := s.Directory(t.Context(), owner, owner, "", 10)
	if err != nil || len(page.Records) != 1 || page.Records[0].Authority != fabricstore.AuthorityHub {
		t.Fatal(page, err)
	}
	raw, err := json.Marshal(page.Records[0])
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Authority string `json:"authority"`
	}
	if json.Unmarshal(raw, &wire) != nil || wire.Authority != "hub" {
		t.Fatal("authority not shown")
	}
	if err := s.RetireActor(t.Context(), owner, actorURN, actor.Version); err != nil {
		t.Fatal(err)
	}
	after, err := repo.Actor(t.Context(), actorURN)
	if err != nil || after.Value.Authority != fabricstore.AuthorityHub || after.Value.Lifecycle != mesh.EnrollmentRetired {
		t.Fatal(after, err)
	}
}

func TestEnrollmentAuthorityDefaultsAndRejectsUnknown(t *testing.T) {
	s, repo, _ := harness(t)
	r := request(actorURN)
	r.Authority = "unknown"
	if err := s.EnrollActor(t.Context(), owner, r); !errors.Is(err, fabricstore.ErrInvalid) {
		t.Fatal(err)
	}
	requireNotEnrolled(t, repo, actorURN)
	r.Authority = ""
	if err := s.EnrollActor(t.Context(), owner, r); err != nil {
		t.Fatal(err)
	}
	a, err := repo.Actor(t.Context(), actorURN)
	if err != nil || a.Value.Authority != fabricstore.AuthorityEnvironment {
		t.Fatal(a, err)
	}
}
