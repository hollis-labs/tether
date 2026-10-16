package authissuer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	auth "github.com/hollis-labs/libs/security/auth"
	"github.com/hollis-labs/tether/internal/identity"
)

// localJWKS serves the issuer's own JWKS to a go-auth JWTVerifier without a
// network hop, so Tether verifies its own tokens through exactly the code the
// other apps run, and a rotation is picked up through the same unknown-kid
// refetch path.
type localJWKS struct{ keys *KeyStore }

const localJWKSURL = "http://tether.invalid/.well-known/jwks.json"

func (l localJWKS) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(bytes.NewReader(l.keys.JWKS()))}, nil
}

// Verifier resolves both credential families to an identity.Principal: stored
// tth_ credentials through Stored, issued JWTs (aud "tether") through go-auth.
// It satisfies identity.Verifier.
type Verifier struct {
	Stored identity.Verifier
	JWT    *auth.JWTVerifier
	// Issuer is the iss every accepted JWT carries (the JWT verifier pins it).
	Issuer string
}

// NewVerifier builds the composite verifier for the issuer's own audience.
func NewVerifier(stored identity.Verifier, is *Issuer) (*Verifier, error) {
	jv, err := auth.NewJWTVerifier(auth.JWTConfig{
		Issuer: is.Name, Audience: TetherAudience, JWKSURL: localJWKSURL,
		Client: &http.Client{Transport: localJWKS{is.Keys}}, MinRefresh: time.Second,
		DenyList: is.State.List,
	})
	if err != nil {
		return nil, err
	}
	return &Verifier{Stored: stored, JWT: jv, Issuer: is.Name}, nil
}

// Verify implements identity.Verifier.
func (v *Verifier) Verify(ctx context.Context, token string) (identity.Principal, error) {
	if strings.HasPrefix(token, "tth_") || strings.Count(token, ".") != 2 {
		if v.Stored == nil {
			return identity.Principal{}, errors.New("identity store unavailable")
		}
		return v.Stored.Verify(ctx, token)
	}
	p, err := v.JWT.Verify(ctx, token)
	switch {
	case err == nil:
	case errors.Is(err, auth.ErrInvalidToken):
		return identity.Principal{}, identity.ErrInvalidToken
	default:
		return identity.Principal{}, err
	}
	exp := p.ExpiresAt
	display := p.Display
	if display == "" {
		display = "Issued token for " + p.ID
	}
	return identity.Principal{Verified: true, ID: p.ID, Kind: p.Kind, Display: display, Scopes: append([]string(nil), p.Scopes...),
		SessionID: p.SessionID, Issuer: v.Issuer, Audience: p.Audience, TokenID: p.TokenID, Parent: p.Parent, ExpiresAt: &exp, Addresses: []string{p.ID}}, nil
}
