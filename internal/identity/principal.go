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

type Principal struct {
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
