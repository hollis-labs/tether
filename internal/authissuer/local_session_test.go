package authissuer

import (
	"context"
	"github.com/hollis-labs/tether/internal/identity"
	"testing"
	"time"
)

func TestLocalSessionMintPreservesCeilingExpiryAndRevocation(t *testing.T) {
	issuer, _ := newIssuer(t)
	now := time.Now().UTC().Truncate(time.Second)
	issuer.Now = func() time.Time { return now }
	expires := now.Add(5 * time.Minute)
	p := identity.Principal{Verified: true, Kind: "session", ID: SessionPrincipalID("local"), SessionID: "local", Scopes: []string{"session.write", "message.write"}, ExpiresAt: &expires}
	token, err := issuer.MintLocalSession(p)
	if err != nil {
		t.Fatal(err)
	}
	claims := claimsOf(t, token.Token)
	if claims.Aud != "tether" || claims.Kind != "agent" || claims.SID != "local" || claims.Sub != p.ID || claims.Scope != "tether.messages tether.sessions" || claims.Exp != expires.Unix() {
		t.Fatal("local lease lost binding or ceiling")
	}
	verifier, err := NewVerifier(nil, issuer)
	if err != nil {
		t.Fatal(err)
	}
	if verified, err := verifier.Verify(context.Background(), token.Token); err != nil || verified.SessionID != "local" {
		t.Fatal("JWT not verifiable", err)
	}
	if err := issuer.RevokeSession("local"); err != nil {
		t.Fatal(err)
	}
	if _, err := issuer.MintLocalSession(p); err == nil {
		t.Fatal("revoked session minted")
	}
}

func TestLocalSessionMintRefusesOtherCredentialFamilies(t *testing.T) {
	issuer, _ := newIssuer(t)
	exp := time.Now().Add(time.Hour)
	valid := identity.Principal{Verified: true, Kind: "session", ID: SessionPrincipalID("local"), SessionID: "local", Scopes: []string{"message.write"}, ExpiresAt: &exp}
	for name, change := range map[string]func(*identity.Principal){
		"unverified":      func(p *identity.Principal) { p.Verified = false },
		"issued":          func(p *identity.Principal) { p.TokenID = "issued-jti" },
		"wrong subject":   func(p *identity.Principal) { p.ID = "foreign" },
		"agent":           func(p *identity.Principal) { p.Kind = "agent" },
		"unbounded lease": func(p *identity.Principal) { p.ExpiresAt = nil },
		"unknown scopes":  func(p *identity.Principal) { p.Scopes = []string{"not.a_scope"} },
		"empty grant":     func(p *identity.Principal) { p.Scopes = nil },
	} {
		t.Run(name, func(t *testing.T) {
			p := valid
			change(&p)
			if _, err := issuer.MintLocalSession(p); err == nil {
				t.Fatal("credential minted")
			}
		})
	}
	tok, err := issuer.MintLocalSession(valid)
	if err != nil {
		t.Fatal(err)
	}
	if time.Until(tok.ExpiresAt) > DefaultTTL {
		t.Fatal("unbounded JWT lifetime")
	}
}
