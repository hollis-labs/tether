// Package runtoken implements launcher-signed correlation, separate from bearer authorization.
package runtoken

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"
)

const Header = "X-Run-Token"
const TokenEnv = "TETHER_RUN_TOKEN"
const IDEnv = "TETHER_RUN_ID"
const MaxSize = 4096

var ErrInvalid = errors.New("invalid run token")

// Claims contains only correlation identifiers. It confers no authorization.
type Claims struct {
	URN        string `json:"urn"`
	RunID      string `json:"run_id"`
	Session    string `json:"session"`
	Task       string `json:"task"`
	Workstream string `json:"workstream"`
	Parent     string `json:"parent"`
	Expires    int64  `json:"exp"`
	JTI        string `json:"jti"`
	Issuer     string `json:"iss"`
	Audience   string `json:"aud"`
}

const issuer = "tether-launcher"
const audience = "tether-mcp-run"
const header = `{"alg":"EdDSA","typ":"tether-run+jwt"}`

var encoding = base64.RawURLEncoding

func valid(c Claims, now time.Time) bool {
	if c.URN == "" || c.Session == "" || c.RunID == "" || c.JTI == "" || c.Expires <= now.Unix() || c.Issuer != issuer || c.Audience != audience {
		return false
	}
	for _, s := range []string{c.URN, c.Session, c.RunID, c.JTI, c.Task, c.Workstream, c.Parent} {
		if len(s) > 512 || strings.ContainsAny(s, "\r\n\x00") {
			return false
		}
	}
	return true
}

func Sign(key ed25519.PrivateKey, c Claims) (string, error) {
	c.Issuer, c.Audience = issuer, audience
	if len(key) != ed25519.PrivateKeySize || !valid(c, time.Now()) {
		return "", ErrInvalid
	}
	b, err := json.Marshal(c)
	if err != nil {
		return "", ErrInvalid
	}
	payload := encoding.EncodeToString([]byte(header)) + "." + encoding.EncodeToString(b)
	token := payload + "." + encoding.EncodeToString(ed25519.Sign(key, []byte(payload)))
	if len(token) > MaxSize {
		return "", ErrInvalid
	}
	return token, nil
}

func Verify(key ed25519.PublicKey, token string, now time.Time) (Claims, error) {
	var c Claims
	if len(key) != ed25519.PublicKeySize || len(token) > MaxSize {
		return c, ErrInvalid
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return c, ErrInvalid
	}
	h, e := encoding.DecodeString(parts[0])
	if e != nil || string(h) != header {
		return c, ErrInvalid
	}
	sig, e := encoding.DecodeString(parts[2])
	if e != nil || !ed25519.Verify(key, []byte(parts[0]+"."+parts[1]), sig) {
		return c, ErrInvalid
	}
	b, e := encoding.DecodeString(parts[1])
	if e != nil || json.Unmarshal(b, &c) != nil || !valid(c, now) {
		return Claims{}, ErrInvalid
	}
	return c, nil
}

// Authority caches local key material. No network service is used to mint or verify.
// The singleton key survives daemon restart in the credential-protected state DB.
// Tokens themselves are never stored. Rotation intentionally requires an operator migration.
type Authority struct {
	once sync.Once
	key  ed25519.PrivateKey
	err  error
}

func (a *Authority) Key(db *sql.DB) (ed25519.PrivateKey, error) {
	a.once.Do(func() {
		if db == nil {
			a.err = ErrInvalid
			return
		}
		_, key, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			a.err = err
			return
		}
		_, err = db.Exec(`INSERT OR IGNORE INTO run_signing_key(id, private_key) VALUES (1, ?)`, []byte(key))
		if err != nil {
			a.err = err
			return
		}
		var raw []byte
		a.err = db.QueryRow(`SELECT private_key FROM run_signing_key WHERE id=1`).Scan(&raw)
		if a.err == nil {
			if len(raw) != ed25519.PrivateKeySize {
				a.err = ErrInvalid
			} else {
				a.key = ed25519.PrivateKey(raw)
			}
		}
	})
	return a.key, a.err
}

// FromHeader distinguishes an absent header from a supplied empty/duplicate header.
func FromHeader(h http.Header) (string, error) {
	v := h.Values(Header)
	if len(v) == 0 {
		return "", nil
	}
	if len(v) != 1 || v[0] == "" || len(v[0]) > MaxSize {
		return "", ErrInvalid
	}
	return v[0], nil
}

type contextKey struct{}

func WithClaims(ctx context.Context, c Claims) context.Context {
	return context.WithValue(ctx, contextKey{}, c)
}
func FromContext(ctx context.Context) (Claims, bool) {
	c, ok := ctx.Value(contextKey{}).(Claims)
	return c, ok
}
