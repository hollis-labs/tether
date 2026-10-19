package enrollment

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"encoding/base64"
	"strconv"
	"time"
)

const DeliverySignatureProfile = "tether-enrollment-delivery-v2+sig"

// SignedDelivery authenticates the entire sealed envelope, including its key ID.
// Neither a caller-supplied issuer key nor a key URL is accepted in this format.
type SignedDelivery struct {
	Profile   string   `json:"profile"`
	KeyID     string   `json:"kid"`
	Delivery  Delivery `json:"delivery"`
	Signature string   `json:"signature"`
}

// DeliveryTrust is a snapshot supplied by the helper's pinned issuer trust
// loader, never by the delivery response. Both JWKS and emergency controls must
// have been checked successfully; a failed refresh must invalidate the snapshot.
// The caller must synchronize snapshots with concurrent trust updates.
type DeliveryTrust struct {
	Issuer             string
	Keys               map[string]ed25519.PublicKey
	KeysCheckedAt      time.Time
	EmergencyCheckedAt time.Time
	Enabled            bool
	DeniedKeyIDs       map[string]bool
}

// sealSigned is deliberately private. The future durable controller must load
// the approved operation and key from its own store; there is no exported
// issuer-signing API accepting arbitrary callers' claims or envelopes.
func sealSigned(private ed25519.PrivateKey, keyID string, keys DestinationKeys, binding Binding, credentialID int64, credential []byte, now, expires time.Time) (SignedDelivery, error) {
	if len(private) != ed25519.PrivateKeySize || !validID(keyID) || !strongSigningKey(private.Public().(ed25519.PublicKey)) {
		return SignedDelivery{}, ErrCustody
	}
	d, err := seal(keys, binding, credentialID, credential, now, expires)
	if err != nil {
		return SignedDelivery{}, err
	}
	signed := SignedDelivery{Profile: DeliverySignatureProfile, KeyID: keyID, Delivery: d}
	signed.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(private, signedDeliveryInfo(signed)))
	return signed, nil
}

// OpenSigned verifies current pinned issuer trust and the issuer's signature
// before decrypting. There is no exported unsigned opening path. The helper
// must still privately exchange the credential and verify the exact approved
// subject/scopes before storing and confirming; this function is not enrollment
// authorization, credential validation, persistence or custody confirmation.
func OpenSigned(trust DeliveryTrust, keys DestinationKeys, private *ecdh.PrivateKey, expected Binding, expectedCredentialID int64, signed SignedDelivery, now time.Time) ([]byte, error) {
	if !trust.Enabled || trust.Issuer != expected.Issuer || signed.Profile != DeliverySignatureProfile ||
		!validID(signed.KeyID) || trust.DeniedKeyIDs[signed.KeyID] ||
		!fresh(trust.KeysCheckedAt, now, time.Minute) || !fresh(trust.EmergencyCheckedAt, now, 5*time.Second) ||
		signed.Delivery.Binding != expected || signed.Delivery.CredentialID != expectedCredentialID ||
		!validDelivery(signed.Delivery, now, DestinationClockSkew) ||
		len(signed.Delivery.Ciphertext) > base64.RawURLEncoding.EncodedLen(MaxCredentialBytes+48) {
		return nil, ErrCustody
	}
	pub := trust.Keys[signed.KeyID]
	if !strongSigningKey(pub) {
		return nil, ErrCustody
	}
	sig, err := decode(signed.Signature, ed25519.SignatureSize)
	if err != nil || !ed25519.Verify(pub, signedDeliveryInfo(signed), sig) {
		return nil, ErrCustody
	}
	return open(keys, private, expected, signed.Delivery, now)
}

func fresh(checked, now time.Time, maxAge time.Duration) bool {
	return !checked.IsZero() && !checked.After(now) && now.Sub(checked) < maxAge
}

func signedDeliveryInfo(d SignedDelivery) []byte {
	return transcript(DeliverySignatureProfile, d.Delivery.Binding, d.KeyID, d.Delivery.Profile,
		strconv.FormatInt(d.Delivery.CredentialID, 10), strconv.FormatInt(d.Delivery.IssuedAt, 10),
		strconv.FormatInt(d.Delivery.ExpiresAt, 10), d.Delivery.Ciphertext)
}
