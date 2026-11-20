package authissuer

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
)

const catalogWindowDomain = "tether.catalog.breakglass.v1\x00"

// SealCatalogBreakglass signs an internal state record with the existing active
// issuer key. It never mints a token or generates another key. Key rotation
// invalidates outstanding records rather than extending their authority.
func (ks *KeyStore) SealCatalogBreakglass(body []byte) (string, error) {
	_, key := ks.Active()
	return base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, append([]byte(catalogWindowDomain), body...))), nil
}

// VerifyCatalogBreakglass accepts only the current issuer trust anchor.
func (ks *KeyStore) VerifyCatalogBreakglass(body []byte, signature string) error {
	sig, err := base64.RawURLEncoding.DecodeString(signature)
	if err != nil {
		return errors.New("invalid catalog breakglass signature")
	}
	_, key := ks.Active()
	if !ed25519.Verify(key.Public().(ed25519.PublicKey), append([]byte(catalogWindowDomain), body...), sig) {
		return errors.New("catalog breakglass signature verification failed")
	}
	return nil
}
