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
	"strings"
	"time"
)

var ErrInvalidToken = errors.New("invalid or inactive identity token")

const OperatorID = "msg://user/local/operator"

// Principal is an identity row bound to a bearer credential. Verified is the
// single definition of "verified" (RK-044): the request carried a bearer
// credential bound to an authenticated principal row, of any kind (operator,
// session, device, service, interactive). It is set only where a credential
// verifies (Store.Verify, VerifyTx, and the middleware for an injected
// Verifier); WithPrincipal preserves it and never sets it. A Principal built from a claim (?as=, headers, JSON) is never
// verified. Every other "verified" field derives from this one. It says who
// the credential belongs to, not that the caller may do anything: scope policy
// is separate, and trusting the flag against same-uid agents additionally
// needs read protection of the credential (CW-20261001-0263).
type Principal struct {
	Verified  bool     `json:"verified"`
	ID        string   `json:"id"`
	Kind      string   `json:"kind"`
	Display   string   `json:"display"`
	Scopes    []string `json:"scopes"`
	SessionID string   `json:"session_id,omitempty"`
	// Audience, TokenID and Parent are set only for principals resolved from
	// an issued JWT (authissuer): the aud and jti claims and the minting
	// parent. A non-empty TokenID is how the daemon tells an issued token
	// from a stored tth_ credential.
	Audience  string     `json:"audience,omitempty"`
	TokenID   string     `json:"token_id,omitempty"`
	Parent    string     `json:"parent,omitempty"`
	Addresses []string   `json:"addresses"`
	CreatedBy string     `json:"created_by,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

type principalKey struct{}

// WithPrincipal records a principal for attribution and preserves its Verified
// flag as given. It never sets it: an internally constructed principal (for
// example the turn router's session sender) stays unverified.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
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

// ScopeSendAs lets a principal author a sender or mailbox address outside its
// own address set. The local operator holds it through "*".
const ScopeSendAs = "message.send_as"

// MayActAs reports whether p may author or read as addr: addr is in the
// principal's address set (its ID plus Addresses), or p holds message.send_as.
func (p Principal) MayActAs(addr string) bool {
	for _, a := range p.Scopes {
		if a == "*" || a == ScopeSendAs {
			return true
		}
	}
	if addr == "" {
		return false
	}
	if addr == p.ID || addr == p.DefaultAddress() {
		return true
	}
	for _, a := range p.Addresses {
		if a == addr {
			return true
		}
	}
	return false
}

// DefaultAddress is the address the daemon stamps as sender when a verified
// principal sends without a from: its first address, else its ID. A device ID
// (msg://device/<id>) is not a valid messaging address, so a device with no
// registered address sends as msg://service/device/<id>.
func (p Principal) DefaultAddress() string {
	if len(p.Addresses) > 0 {
		return p.Addresses[0]
	}
	if rest, ok := strings.CutPrefix(p.ID, "msg://device/"); ok && rest != "" {
		return "msg://service/device/" + rest
	}
	return p.ID
}

// IsOperatorCredential reports whether p is the real local operator
// credential. An issued token minted by the operator (TokenID set) is narrowed
// by its scopes and is never the operator itself, even though it carries the
// operator's id and kind.
func IsOperatorCredential(p Principal) bool {
	return p.Verified && p.Kind == "operator" && p.ID == OperatorID && p.TokenID == ""
}
