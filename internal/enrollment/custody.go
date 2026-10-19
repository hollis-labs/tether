// Package enrollment contains the closed destination-custody primitives for
// credential enrollment. They neither authorize enrollment nor mint credentials.
// Callers must load approved bindings from authoritative state, consume nonces
// transactionally, and retain the original sealed delivery for retries.
package enrollment

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/hpke"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	DeliveryProfile       = "tether-enrollment-delivery-v1"
	PossessionProfile     = "tether-enrollment-possession-v1"
	MaxCredentialBytes    = 4096
	MaxDeliveryLifetime   = 10 * time.Minute
	MaxPossessionLifetime = time.Minute
)

// ErrCustody deliberately does not disclose which binding or key was wrong.
var ErrCustody = errors.New("invalid enrollment custody evidence")

// DestinationKeys contains only canonical unpadded base64url public keys.
// Signing is Ed25519 and encryption is X25519; algorithm negotiation is absent.
type DestinationKeys struct {
	Signing    string `json:"signing"`
	Encryption string `json:"encryption"`
}

// Binding must come from the approved, immutable server-owned operation and
// custody record, never from the redemption request itself. Thumbprints use
// RFC7638 canonical OKP JWK members and SHA256, encoded as unpadded base64url.
type Binding struct {
	GrantID              string `json:"grant_id"`
	OperationDigest      string `json:"op_digest"`
	PrincipalID          string `json:"principal_id"`
	DestinationID        string `json:"destination_id"`
	DestinationRevision  string `json:"destination_revision"`
	SigningThumbprint    string `json:"signing_thumbprint"`
	EncryptionThumbprint string `json:"encryption_thumbprint"`
}

// Delivery is safe to retain as ciphertext until confirmation or orphan expiry.
// Retrying delivery must return this exact persisted envelope, not call Seal
// again or mint another credential. HPKE base mode is not sender authentication;
// the recipient must obtain this envelope through the authenticated issuer path.
type Delivery struct {
	Profile      string  `json:"profile"`
	Binding      Binding `json:"binding"`
	CredentialID int64   `json:"credential_id"`
	IssuedAt     int64   `json:"issued_at"`
	ExpiresAt    int64   `json:"expires_at"`
	Ciphertext   string  `json:"ciphertext"`
}

// Challenge is server-created and retained with its single-use nonce. Verify
// proves possession only; it does not consume the nonce or approve an operation.
type Challenge struct {
	Profile      string  `json:"profile"`
	Binding      Binding `json:"binding"`
	Action       string  `json:"action"`
	CredentialID int64   `json:"credential_id"`
	Nonce        string  `json:"nonce"`
	IssuedAt     int64   `json:"issued_at"`
	ExpiresAt    int64   `json:"expires_at"`
}

// PublicKeys validates destination keys and returns their RFC7638 thumbprints.
func PublicKeys(keys DestinationKeys) (signingThumbprint, encryptionThumbprint string, err error) {
	sign, err := decode(keys.Signing, ed25519.PublicKeySize)
	if err != nil || allZero(sign) {
		return "", "", ErrCustody
	}
	enc, err := decode(keys.Encryption, 32)
	if err != nil {
		return "", "", ErrCustody
	}
	pub, err := ecdh.X25519().NewPublicKey(enc)
	if err != nil {
		return "", "", ErrCustody
	}
	// ecdh.NewPublicKey checks the length only. Reject non-contributory keys
	// before an operation can be prepared, rather than after credential minting.
	probe, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	if _, err := probe.ECDH(pub); err != nil {
		return "", "", ErrCustody
	}
	return thumbprint("Ed25519", keys.Signing), thumbprint("X25519", keys.Encryption), nil
}

func thumbprint(curve, key string) string {
	// All values are validated base64url or fixed ASCII, so this is exactly
	// the canonical RFC7638 member order, with no JSON escaping ambiguity.
	digest := sha256.Sum256([]byte(`{"crv":"` + curve + `","kty":"OKP","x":"` + key + `"}`))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

func validateBinding(keys DestinationKeys, b Binding) error {
	for _, id := range []string{b.GrantID, b.PrincipalID, b.DestinationID} {
		if !validID(id) {
			return ErrCustody
		}
	}
	revision, err := strconv.ParseUint(b.DestinationRevision, 10, 64)
	if err != nil || revision == 0 || strconv.FormatUint(revision, 10) != b.DestinationRevision {
		return ErrCustody
	}
	if !strings.HasPrefix(b.OperationDigest, "sha256:") || len(b.OperationDigest) != 71 {
		return ErrCustody
	}
	digest, err := hex.DecodeString(b.OperationDigest[7:])
	if err != nil || hex.EncodeToString(digest) != b.OperationDigest[7:] {
		return ErrCustody
	}
	sign, enc, err := PublicKeys(keys)
	if err != nil {
		return err
	}
	if sign != b.SigningThumbprint || enc != b.EncryptionThumbprint {
		return ErrCustody
	}
	return nil
}

// Seal creates an HPKE base-mode delivery using DHKEM(X25519, HKDF-SHA256),
// HKDF-SHA256 and AES-256-GCM. The complete binding and delivery metadata are
// HPKE info, so changing any field prevents opening. No raw key is retained.
func Seal(keys DestinationKeys, b Binding, credentialID int64, credential []byte, now, expires time.Time) (Delivery, error) {
	if err := validateBinding(keys, b); err != nil {
		return Delivery{}, err
	}
	d := Delivery{Profile: DeliveryProfile, Binding: b, CredentialID: credentialID, IssuedAt: now.Unix(), ExpiresAt: expires.Unix()}
	if !validDelivery(d, now) || len(credential) == 0 || len(credential) > MaxCredentialBytes {
		return Delivery{}, ErrCustody
	}
	pubBytes, _ := decode(keys.Encryption, 32)
	pub, err := hpke.DHKEM(ecdh.X25519()).NewPublicKey(pubBytes)
	if err != nil {
		return Delivery{}, ErrCustody
	}
	ciphertext, err := hpke.Seal(pub, hpke.HKDFSHA256(), hpke.AES256GCM(), deliveryInfo(d), credential)
	if err != nil {
		return Delivery{}, ErrCustody
	}
	d.Ciphertext = base64.RawURLEncoding.EncodeToString(ciphertext)
	return d, nil
}

// Open verifies the expected server binding and delivery lifetime before
// decrypting. The caller must additionally verify the authenticated source,
// expected credential ID, current grant state and destination file custody.
func Open(keys DestinationKeys, private *ecdh.PrivateKey, expected Binding, d Delivery, now time.Time) ([]byte, error) {
	if d.Binding != expected || !validDelivery(d, now) || private == nil || private.Curve() != ecdh.X25519() {
		return nil, ErrCustody
	}
	if err := validateBinding(keys, expected); err != nil {
		return nil, err
	}
	if base64.RawURLEncoding.EncodeToString(private.PublicKey().Bytes()) != keys.Encryption {
		return nil, ErrCustody
	}
	// X25519 encapsulation is 32 bytes; AES-GCM adds a 16-byte tag.
	if len(d.Ciphertext) > base64.RawURLEncoding.EncodedLen(MaxCredentialBytes+48) {
		return nil, ErrCustody
	}
	ciphertext, err := base64.RawURLEncoding.DecodeString(d.Ciphertext)
	if err != nil || base64.RawURLEncoding.EncodeToString(ciphertext) != d.Ciphertext || len(ciphertext) <= 48 {
		return nil, ErrCustody
	}
	priv, err := hpke.NewDHKEMPrivateKey(private)
	if err != nil {
		return nil, ErrCustody
	}
	plaintext, err := hpke.Open(priv, hpke.HKDFSHA256(), hpke.AES256GCM(), deliveryInfo(d), ciphertext)
	if err != nil {
		return nil, ErrCustody
	}
	return plaintext, nil
}

func validDelivery(d Delivery, now time.Time) bool {
	return d.Profile == DeliveryProfile && d.CredentialID > 0 && validWindow(d.IssuedAt, d.ExpiresAt, now, MaxDeliveryLifetime)
}

// NewChallenge creates a fresh server nonce. A caller must durably store the
// returned challenge before returning it, bound to the approved grant state.
func NewChallenge(keys DestinationKeys, b Binding, action string, credentialID int64, now time.Time) (Challenge, error) {
	if err := validateBinding(keys, b); err != nil {
		return Challenge{}, err
	}
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return Challenge{}, err
	}
	c := Challenge{Profile: PossessionProfile, Binding: b, Action: action, CredentialID: credentialID, Nonce: base64.RawURLEncoding.EncodeToString(nonce[:]), IssuedAt: now.Unix(), ExpiresAt: now.Add(MaxPossessionLifetime).Unix()}
	if !validChallenge(c, now) {
		return Challenge{}, ErrCustody
	}
	return c, nil
}

// Sign is used only at the destination after checking the issuer challenge
// against the locally retained approved binding. It returns a signature, not a
// credential or reusable redemption bearer.
func Sign(private ed25519.PrivateKey, keys DestinationKeys, expected Binding, c Challenge, now time.Time) (string, error) {
	if len(private) != ed25519.PrivateKeySize || c.Binding != expected || !validChallenge(c, now) {
		return "", ErrCustody
	}
	if err := validateBinding(keys, expected); err != nil {
		return "", err
	}
	if base64.RawURLEncoding.EncodeToString(private.Public().(ed25519.PublicKey)) != keys.Signing {
		return "", ErrCustody
	}
	return base64.RawURLEncoding.EncodeToString(ed25519.Sign(private, challengeInfo(c))), nil
}

// Verify accepts a server-retained challenge, never a challenge loaded from the
// caller's request. The caller must atomically consume its nonce with the state
// transition; repeating this stateless verification alone does not stop replay.
func Verify(keys DestinationKeys, expected Challenge, signature string, now time.Time) error {
	if !validChallenge(expected, now) {
		return ErrCustody
	}
	if err := validateBinding(keys, expected.Binding); err != nil {
		return err
	}
	sig, err := decode(signature, ed25519.SignatureSize)
	if err != nil {
		return ErrCustody
	}
	pub, _ := decode(keys.Signing, ed25519.PublicKeySize)
	if !ed25519.Verify(pub, challengeInfo(expected), sig) {
		return ErrCustody
	}
	return nil
}

func validChallenge(c Challenge, now time.Time) bool {
	_, err := decode(c.Nonce, 32)
	return err == nil && c.Profile == PossessionProfile && validWindow(c.IssuedAt, c.ExpiresAt, now, MaxPossessionLifetime) &&
		((c.Action == "redeem" && c.CredentialID == 0) || (c.Action == "confirm" && c.CredentialID > 0))
}

func validWindow(issued, expires int64, now time.Time, maxLifetime time.Duration) bool {
	// Subtract only after proving ordered positive timestamps, preventing
	// overflow from a malformed far-future expiry. No positive expiry leeway.
	return issued > 0 && expires > issued && expires-issued <= int64(maxLifetime/time.Second) && issued <= now.Unix() && now.Unix() < expires
}

func deliveryInfo(d Delivery) []byte {
	return transcript(DeliveryProfile, d.Binding, strconv.FormatInt(d.CredentialID, 10), strconv.FormatInt(d.IssuedAt, 10), strconv.FormatInt(d.ExpiresAt, 10))
}

func challengeInfo(c Challenge) []byte {
	return transcript(PossessionProfile, c.Binding, c.Action, strconv.FormatInt(c.CredentialID, 10), c.Nonce, strconv.FormatInt(c.IssuedAt, 10), strconv.FormatInt(c.ExpiresAt, 10))
}

// Transcript is a fixed domain + NUL followed by uint32 big-endian byte lengths
// and UTF-8 values in this order. It is a private custody protocol, not the JCS
// operation representation or the shared WebAuthn challenge.
func transcript(domain string, b Binding, tail ...string) []byte {
	out := append([]byte(domain), 0)
	fields := []string{b.GrantID, b.OperationDigest, b.PrincipalID, b.DestinationID, b.DestinationRevision, b.SigningThumbprint, b.EncryptionThumbprint}
	fields = append(fields, tail...)
	for _, field := range fields {
		// All callers validate IDs to 256 bytes; other fields are fixed-size
		// digests, nonces, actions or decimal integers. No unbounded input
		// reaches this conversion.
		// #nosec G115 -- validated transcript fields are at most 256 bytes.
		out = binary.BigEndian.AppendUint32(out, uint32(len(field)))
		out = append(out, field...)
	}
	return out
}

func decode(raw string, size int) ([]byte, error) {
	if len(raw) != base64.RawURLEncoding.EncodedLen(size) {
		return nil, ErrCustody
	}
	value, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(value) != size || base64.RawURLEncoding.EncodeToString(value) != raw {
		return nil, ErrCustody
	}
	return value, nil
}

func validID(id string) bool {
	return id != "" && len(id) <= 256 && utf8.ValidString(id) && !strings.ContainsFunc(id, unicode.IsControl)
}

func allZero(value []byte) bool {
	for _, b := range value {
		if b != 0 {
			return false
		}
	}
	return true
}
