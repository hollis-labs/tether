package authissuer

import (
	"github.com/google/uuid"
	"github.com/hollis-labs/tether/internal/identity"
	"strings"
	"time"
)

// MintLocalSession is an in-process launcher operation, deliberately absent
// from /auth/token. The caller freshly verifies its own stored launch credential
// before every refresh. Issued JWTs cannot use this to mint or extend a lease.
func (i *Issuer) MintLocalSession(p identity.Principal) (Token, error) {
	if !p.Verified || p.Kind != "session" || p.TokenID != "" || p.SessionID == "" || p.ID != SessionPrincipalID(p.SessionID) || p.ExpiresAt == nil || !sidRe.MatchString(p.SessionID) {
		return Token{}, forbidden("local session launch credential required")
	}
	scopes, err := identity.CanonicalScopes(p.Scopes)
	if err != nil || len(scopes) == 0 {
		return Token{}, forbidden("local session has no canonical grant")
	}
	if i.State.Denied(DenySID, p.SessionID) {
		return Token{}, forbidden("session has ended")
	}
	now := i.Now().UTC()
	exp := minTime(now.Add(DefaultTTL), *p.ExpiresAt)
	if !exp.After(now.Add(time.Second)) {
		return Token{}, forbidden("session credential is about to expire")
	}
	c := Claims{Iss: i.Name, Sub: p.ID, Aud: TetherAudience, Scope: strings.Join(scopes, " "), Iat: now.Unix(), Exp: exp.Unix(), JTI: uuid.NewString(), Kind: "agent", SID: p.SessionID, Par: p.ID}
	tok, kid, err := i.sign(c)
	if err != nil {
		return Token{}, err
	}
	return Token{Token: tok, ExpiresAt: exp, TTL: int(exp.Sub(now).Seconds()), Audience: c.Aud, Scopes: scopes, Kind: c.Kind, KID: kid, JTI: c.JTI}, nil
}
func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
