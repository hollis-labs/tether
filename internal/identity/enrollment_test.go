package identity_test

import (
	"context"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/identity"
)

func TestReservedIssuerSubjectsCannotRegister(t *testing.T) {
	s, _ := identityStore(t)
	exp := time.Now().Add(time.Hour)
	for _, kind := range []string{"operator", "service", "interactive", "device", "session"} {
		p := identity.Principal{ID: identity.IssuerSubjectPrefix + "credential-enrollment", Kind: kind, Scopes: []string{"read"}, ExpiresAt: &exp}
		if kind == "session" {
			p.SessionID = "synthetic"
		}
		if token, err := s.Mint(context.Background(), p); err == nil || token != "" {
			t.Fatal("registered reserved issuer principal")
		}
	}
	token, err := s.Mint(context.Background(), identity.Principal{ID: "ordinary", Kind: "service", Scopes: []string{"read"}})
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.Verify(context.Background(), token)
	if err != nil || p.ID != "ordinary" {
		t.Fatal("ordinary registration changed")
	}
}

func TestIssuerSubjectReservationIgnoresCase(t *testing.T) {
	s, _ := identityStore(t)
	for _, id := range []string{"URN:TETHER:ISSUER:credential-enrollment", "uRn:TeThEr:IsSuEr:other"} {
		if !identity.ReservedIssuerSubject(id) {
			t.Fatal("alternate-case issuer subject not reserved")
		}
		if token, err := s.Mint(context.Background(), identity.Principal{ID: id, Kind: "service", Scopes: []string{"read"}}); err == nil || token != "" {
			t.Fatal("alternate-case issuer subject registered")
		}
	}
}
