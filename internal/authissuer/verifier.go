package authissuer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
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
	// SessionAddresses, when set, returns the addresses recorded on a session's live
	// stored credential. An agent JWT is bound to a session by its sid claim but carries
	// only the session id; without this its principal could not author as the agent the
	// session was launched to act as, and enforce mode would refuse every message it sends.
	SessionAddresses func(ctx context.Context, sessionID string) ([]string, error)
}

// VerifierOption adjusts how issued JWTs are verified.
type VerifierOption func(*auth.JWTConfig)

// WithLeeway sets the clock skew allowed for exp/nbf (the library default is 30s). Tests use a tiny value to
// observe an expiry; the daemon keeps the default.
func WithLeeway(d time.Duration) VerifierOption { return func(c *auth.JWTConfig) { c.Leeway = d } }

// NewVerifier builds the composite verifier for the issuer's own audience. It dispatches on the credential's
// format with no fallthrough: a stored "tth_" credential (or anything that is not a three-part JWT) goes to the
// stored store, a JWT goes to issuer verification, and a failure on one path is final: it is never retried on the
// other.
func NewVerifier(stored identity.Verifier, is *Issuer, opts ...VerifierOption) (*Verifier, error) {
	cfg := auth.JWTConfig{
		Issuer: is.Name, Audience: TetherAudience, JWKSURL: localJWKSURL,
		Client: &http.Client{Transport: localJWKS{is.Keys}}, MinRefresh: time.Second,
		DenyList: is.State.List,
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	jv, err := auth.NewJWTVerifier(cfg)
	if err != nil {
		return nil, err
	}
	v := &Verifier{Stored: stored, JWT: jv, Issuer: is.Name}
	if st, ok := stored.(interface {
		SessionAddresses(ctx context.Context, sessionID string) ([]string, error)
	}); ok {
		v.SessionAddresses = st.SessionAddresses
	}
	return v, nil
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
	addresses := []string{p.ID}
	if p.Kind == "agent" && p.SessionID != "" && p.ID == SessionPrincipalID(p.SessionID) && v.SessionAddresses != nil {
		// A lookup failure leaves the session id alone: narrower, never wider.
		if recorded, err := v.SessionAddresses(ctx, p.SessionID); err == nil {
			for _, a := range recorded {
				if a != "" && !slices.Contains(addresses, a) {
					addresses = append(addresses, a)
				}
			}
		}
	}
	return identity.Principal{Verified: true, ID: p.ID, Kind: p.Kind, Display: display, Scopes: append([]string(nil), p.Scopes...),
		SessionID: p.SessionID, Issuer: v.Issuer, Audience: p.Audience, TokenID: p.TokenID, Parent: p.Parent, ExpiresAt: &exp, Addresses: addresses}, nil
}
