package authissuer

import (
	"errors"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/identity"
)

func TestEnrollmentNamespaceCannotUseOrdinaryIssuer(t *testing.T) {
	is, dir := newIssuer(t)
	for _, kind := range []string{"operator", "device", "service"} {
		p := identity.Principal{ID: "ordinary", Kind: kind, Verified: true, Scopes: []string{"*"}}
		for _, aud := range []string{"tachyon-enrollment", "tachyon-enrollment-v2", "tachyon-enrollmentanything"} {
			for _, sid := range []string{"", "session"} {
				if tok, err := is.Exchange(p, Request{Audience: aud, Scopes: []string{aud + ".enqueue"}, SessionID: sid}); !errors.Is(err, ErrForbidden) || tok.Token != "" {
					t.Fatal("ordinary exchange entered reserved profile")
				}
			}
			if err := is.RegisterSessionGrant("session", p.ID, map[string][]string{aud: {aud + ".enqueue"}}, time.Now().Add(time.Hour)); !errors.Is(err, ErrForbidden) {
				t.Fatal("reserved launch grant registered")
			}
			if err := is.State.Grant("session", SessionGrant{Launcher: p.ID, Scopes: map[string][]string{aud: {aud + ".enqueue"}}, Expires: time.Now().Add(time.Hour)}); !errors.Is(err, ErrForbidden) {
				t.Fatal("direct state registration bypassed reservation")
			}
		}
		p.ID = identity.IssuerSubjectPrefix + "credential-enrollment"
		if tok, err := is.Exchange(p, Request{Audience: "tachyon", Scopes: []string{"tachyon.approvals_read"}}); !errors.Is(err, ErrForbidden) || tok.Token != "" {
			t.Fatal("reserved subject used ordinary exchange")
		}
		if err := is.RegisterSessionGrant("session", p.ID, map[string][]string{"tachyon": {"tachyon.approvals_read"}}, time.Now().Add(time.Hour)); !errors.Is(err, ErrForbidden) {
			t.Fatal("reserved subject launched session")
		}
	}
	reopened, err := OpenState(dir + "/state.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reopened.GrantFor("session"); ok {
		t.Fatal("refused grant persisted")
	}
	// Existing normal application exchanges and grants continue to work.
	p := identity.Principal{ID: "ordinary", Kind: "service", Verified: true, Scopes: []string{"tachyon.approvals_read"}}
	if _, err := is.Exchange(p, Request{Audience: "tachyon", Scopes: []string{"tachyon.approvals_read"}}); err != nil {
		t.Fatal(err)
	}
	if err := is.RegisterSessionGrant("normal", p.ID, map[string][]string{"tachyon": {"tachyon.approvals_read"}}, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
}
