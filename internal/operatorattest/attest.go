// Package operatorattest verifies operator-attested replies (CW-20261114-0152,
// docs/operator-attested-replies.md): a Tangent-signed Ed25519 compact JWS over
// a single-use Tether challenge, proving Chrispian stepped up and chose one
// option of one parent message. It is pure: no store, no clock but the one
// passed in.
package operatorattest

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Contract constants.
const (
	Audience  = "tether"
	Subject   = "msg://user/local/operator"
	Algorithm = "EdDSA"
	Type      = "operator-reply+jwt"
	// MaxAgeLimit caps max_age_seconds: the freshness bound is Tether's own and
	// never longer than this, whatever the operator's elevation_ttl says.
	MaxAgeLimit = 300 * time.Second
	// MaxKeys is the rotation overlap.
	MaxKeys = 2
	// Skew is the clock skew allowed on iat and elevated_at.
	Skew = 30 * time.Second
)

// Failure classes, one per contract error code.
var (
	ErrInvalid = errors.New("assertion_invalid") // malformed, wrong alg/typ, unknown kid, bad signature
	ErrStale   = errors.New("assertion_stale")   // a freshness rule failed
	ErrUnbound = errors.New("assertion_unbound") // a binding rule failed
)

// Key is one approved Tangent public key.
type Key struct {
	ID     string
	Public ed25519.PublicKey
}

// Config is the approved operator_attestation catalog block, parsed.
type Config struct {
	Issuer string
	MaxAge time.Duration
	Keys   []Key
}

// RawKey and RawConfig are the catalog's YAML shape.
type RawKey struct {
	ID        string `yaml:"id" json:"id"`
	PublicKey string `yaml:"public_key" json:"public_key"`
}

type RawConfig struct {
	Issuer        string   `yaml:"issuer" json:"issuer"`
	MaxAgeSeconds int      `yaml:"max_age_seconds" json:"max_age_seconds"`
	Keys          []RawKey `yaml:"keys" json:"keys"`
}

// Parse validates a catalog block: an issuer, 0 < max_age_seconds <= 300, one
// or two keys with distinct ids, each "ed25519:" plus the base64url (unpadded)
// of a 32-byte public key.
func Parse(raw RawConfig) (Config, error) {
	c := Config{Issuer: strings.TrimSpace(raw.Issuer), MaxAge: time.Duration(raw.MaxAgeSeconds) * time.Second}
	if c.Issuer == "" {
		return Config{}, errors.New("operator_attestation.issuer is required")
	}
	if raw.MaxAgeSeconds <= 0 || c.MaxAge > MaxAgeLimit {
		return Config{}, fmt.Errorf("operator_attestation.max_age_seconds must be 1..%d", int(MaxAgeLimit/time.Second))
	}
	if len(raw.Keys) == 0 || len(raw.Keys) > MaxKeys {
		return Config{}, fmt.Errorf("operator_attestation.keys must have 1..%d entries, has %d", MaxKeys, len(raw.Keys))
	}
	seen := map[string]bool{}
	for i, k := range raw.Keys {
		id := strings.TrimSpace(k.ID)
		if id == "" || seen[id] {
			return Config{}, fmt.Errorf("operator_attestation.keys[%d].id is empty or repeated", i)
		}
		seen[id] = true
		enc, ok := strings.CutPrefix(strings.TrimSpace(k.PublicKey), "ed25519:")
		if !ok {
			return Config{}, fmt.Errorf("operator_attestation.keys[%d].public_key lacks the ed25519 prefix", i)
		}
		pub, err := base64.RawURLEncoding.DecodeString(enc)
		if err != nil || len(pub) != ed25519.PublicKeySize {
			return Config{}, fmt.Errorf("operator_attestation.keys[%d].public_key is not the base64url of a 32-byte Ed25519 key", i)
		}
		c.Keys = append(c.Keys, Key{ID: id, Public: ed25519.PublicKey(pub)})
	}
	return c, nil
}

// KeyIDs lists the configured key ids (for audit).
func (c Config) KeyIDs() []string {
	ids := make([]string, 0, len(c.Keys))
	for _, k := range c.Keys {
		ids = append(ids, k.ID)
	}
	return ids
}

// Binding is hex sha256(message_id || 0x00 || option): what is signed, so a
// truncated display cannot change what is approved.
func Binding(messageID, option string) string {
	h := sha256.New()
	h.Write([]byte(messageID))
	h.Write([]byte{0})
	h.Write([]byte(option))
	return hex.EncodeToString(h.Sum(nil))
}

// SHA256Hex is the hex SHA-256 of s (body_sha256, note_sha256, the nonce hash).
func SHA256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// Claims is the assertion's payload.
type Claims struct {
	Iss         string  `json:"iss"`
	Aud         string  `json:"aud"`
	Sub         string  `json:"sub"`
	Jti         string  `json:"jti"`
	Iat         int64   `json:"iat"`
	Exp         int64   `json:"exp"`
	ChallengeID string  `json:"challenge_id"`
	Nonce       string  `json:"nonce"`
	MessageID   string  `json:"message_id"`
	Option      string  `json:"option"`
	Binding     string  `json:"binding"`
	BodySHA256  string  `json:"body_sha256"`
	NoteSHA256  *string `json:"note_sha256,omitempty"`
	DeviceID    string  `json:"device_id"`
	ElevationID string  `json:"elevation_id"`
	ElevatedAt  int64   `json:"elevated_at"`
}

type header struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
	Typ string `json:"typ"`
}

// Verify checks a compact JWS's form and signature against the approved keys
// and returns its claims and key id. Any failure is ErrInvalid.
func Verify(token string, c Config) (Claims, string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Claims{}, "", fmt.Errorf("%w: not a compact JWS", ErrInvalid)
	}
	rawHeader, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return Claims{}, "", fmt.Errorf("%w: header encoding", ErrInvalid)
	}
	var h header
	if err = strictJSON(rawHeader, &h); err != nil {
		return Claims{}, "", fmt.Errorf("%w: header: %w", ErrInvalid, err)
	}
	if h.Alg != Algorithm {
		return Claims{}, "", fmt.Errorf("%w: alg %q", ErrInvalid, h.Alg)
	}
	if h.Typ != Type {
		return Claims{}, "", fmt.Errorf("%w: typ %q", ErrInvalid, h.Typ)
	}
	var key ed25519.PublicKey
	for _, k := range c.Keys {
		if k.ID == h.Kid {
			key = k.Public
		}
	}
	if key == nil {
		return Claims{}, "", fmt.Errorf("%w: kid %q is not an approved key", ErrInvalid, h.Kid)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(sig) != ed25519.SignatureSize {
		return Claims{}, "", fmt.Errorf("%w: signature encoding", ErrInvalid)
	}
	if !ed25519.Verify(key, []byte(parts[0]+"."+parts[1]), sig) {
		return Claims{}, "", fmt.Errorf("%w: bad signature", ErrInvalid)
	}
	rawClaims, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Claims{}, "", fmt.Errorf("%w: payload encoding", ErrInvalid)
	}
	var claims Claims
	if err = strictJSON(rawClaims, &claims); err != nil {
		return Claims{}, "", fmt.Errorf("%w: payload: %w", ErrInvalid, err)
	}
	return claims, h.Kid, nil
}

// strictJSON decodes one JSON object and refuses trailing data.
func strictJSON(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("trailing data")
	}
	return nil
}

// CheckFresh applies the contract's freshness rules (step 4) with Skew. maxAge
// is the approved max_age_seconds; elevation_ttl plays no part.
func CheckFresh(c Claims, now time.Time, maxAge time.Duration) error {
	n := now.Unix()
	limit := int64(maxAge / time.Second)
	skew := int64(Skew / time.Second)
	switch {
	case c.Exp <= n:
		return fmt.Errorf("%w: expired", ErrStale)
	case c.Exp-c.Iat > limit:
		return fmt.Errorf("%w: lifetime exp-iat %ds exceeds %ds", ErrStale, c.Exp-c.Iat, limit)
	case c.Iat > n+skew:
		return fmt.Errorf("%w: iat in the future", ErrStale)
	case n-c.Iat > limit:
		return fmt.Errorf("%w: iat %ds old", ErrStale, n-c.Iat)
	case c.ElevatedAt > n+skew:
		return fmt.Errorf("%w: elevated_at in the future", ErrStale)
	case n-c.ElevatedAt > limit:
		return fmt.Errorf("%w: step-up %ds old exceeds %ds", ErrStale, n-c.ElevatedAt, limit)
	case c.ElevatedAt > c.Iat:
		return fmt.Errorf("%w: elevated_at after iat", ErrStale)
	}
	return nil
}

// Expect is what an assertion must be bound to: the approved issuer, the
// stored challenge, the parent and the request.
type Expect struct {
	Issuer      string
	ChallengeID string
	NonceSHA256 string // hex, as stored
	MessageID   string
	Option      string // the challenge's option; the request's must equal it
	Binding     string
	BodySHA256  string
	DeviceID    string
	Note        *string // the request's note, nil when absent
}

// CheckBinding applies the contract's binding rules (step 5).
func CheckBinding(c Claims, e Expect) error {
	fail := func(what string) error { return fmt.Errorf("%w: %s", ErrUnbound, what) }
	switch {
	case c.Iss != e.Issuer:
		return fail("iss")
	case c.Aud != Audience:
		return fail("aud")
	case c.Sub != Subject:
		return fail("sub")
	case c.Jti == "":
		return fail("jti missing")
	case c.ElevationID == "":
		return fail("elevation_id missing")
	case c.ChallengeID != e.ChallengeID:
		return fail("challenge_id")
	case subtle.ConstantTimeCompare([]byte(SHA256Hex(c.Nonce)), []byte(e.NonceSHA256)) != 1:
		return fail("nonce")
	case c.MessageID != e.MessageID:
		return fail("message_id")
	case c.Option != e.Option:
		return fail("option")
	case c.Binding != e.Binding || c.Binding != Binding(e.MessageID, e.Option):
		return fail("binding")
	case c.BodySHA256 != e.BodySHA256:
		return fail("body_sha256")
	case c.DeviceID != e.DeviceID:
		return fail("device_id")
	}
	switch {
	case e.Note == nil && c.NoteSHA256 != nil:
		return fail("note_sha256 without a note")
	case e.Note != nil && c.NoteSHA256 == nil:
		return fail("note without note_sha256")
	case e.Note != nil && *c.NoteSHA256 != SHA256Hex(*e.Note):
		return fail("note_sha256")
	}
	return nil
}

// Digest is the identity of an approved trust root: the hex SHA-256 of the
// block's canonical JSON, "" for no block. Tether stores it when an operator
// approves a plan that changes the block, and refuses a catalog whose block
// has another digest.
func Digest(raw *RawConfig) string {
	if raw == nil {
		return ""
	}
	b, _ := json.Marshal(raw) // a struct of strings and ints: always marshals
	return SHA256Hex(string(b))
}
