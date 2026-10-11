// Package identity implements daemon-verified bearer identity. Phase 1
// establishes attribution; address stamping and scope policy are separate.
package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

var ErrInvalidToken = errors.New("invalid or inactive identity token")

const OperatorID = "msg://user/local/operator"

// Principal is an identity row bound to a bearer credential. Verified is the
// single definition of "verified" (RK-044): the request carried a bearer
// credential bound to an authenticated principal row, of any kind (operator,
// session, device, service, interactive). It is set only by WithPrincipal,
// which only the authentication middleware reaches after a credential
// verifies; a Principal built from a claim (?as=, headers, JSON) is never
// verified. Every other "verified" field derives from this one. It says who
// the credential belongs to, not that the caller may do anything: scope policy
// is separate, and trusting the flag against same-uid agents additionally
// needs read protection of the credential (CW-20261001-0263).
type Principal struct {
	Verified  bool       `json:"verified"`
	ID        string     `json:"id"`
	Kind      string     `json:"kind"`
	Display   string     `json:"display"`
	Scopes    []string   `json:"scopes"`
	SessionID string     `json:"session_id,omitempty"`
	Addresses []string   `json:"addresses"`
	CreatedBy string     `json:"created_by,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

type principalKey struct{}

// WithPrincipal records a bearer-authenticated principal and is the only place
// Verified becomes true.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	p.Verified = true
	p.Scopes = append([]string(nil), p.Scopes...)
	p.Addresses = append([]string(nil), p.Addresses...)
	return context.WithValue(ctx, principalKey{}, p)
}

func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	p.Scopes = append([]string(nil), p.Scopes...)
	p.Addresses = append([]string(nil), p.Addresses...)
	return p, ok
}

// Verified reports the one verified flag for the request context. An absent
// principal (anonymous or claim-only caller) is not verified.
func Verified(ctx context.Context) bool {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return ok && p.Verified
}

// PolicyPrincipal is the principal contract handed to the policy library: the
// Cedar request carries principal.verified. It holds attribution only, never
// credentials or scopes.
type PolicyPrincipal struct {
	ID       string `json:"id,omitempty"`
	Kind     string `json:"kind,omitempty"`
	Verified bool   `json:"verified"`
}

// PolicyPrincipalFrom derives the policy principal from the request context.
// With no verified principal it returns the anonymous principal (Verified
// false, no id), so a claimed identity can never reach a decision as verified.
func PolicyPrincipalFrom(ctx context.Context) PolicyPrincipal {
	p, ok := FromContext(ctx)
	if !ok || !p.Verified {
		return PolicyPrincipal{}
	}
	return PolicyPrincipal{ID: p.ID, Kind: p.Kind, Verified: true}
}

func NewToken() (string, error) {
	var bytes [32]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", fmt.Errorf("mint identity token: %w", err)
	}
	return "tth_" + base64.RawURLEncoding.EncodeToString(bytes[:]), nil
}

func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func validToken(token string) bool {
	if len(token) != 47 || token[:4] != "tth_" {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(token[4:])
	return err == nil && len(raw) == 32 && base64.RawURLEncoding.EncodeToString(raw) == token[4:]
}
