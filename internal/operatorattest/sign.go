package operatorattest

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
)

// Sign produces the compact JWS Tangent signs (CW-20261114-0153): header
// {alg: EdDSA, kid, typ: operator-reply+jwt} over the claims. Tether only
// verifies; Sign is the reference for Tangent's test vector and for tests.
func Sign(c Claims, kid string, priv ed25519.PrivateKey) (string, error) {
	h, err := json.Marshal(header{Alg: Algorithm, Kid: kid, Typ: Type})
	if err != nil {
		return "", err
	}
	p, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	signing := base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(p)
	return signing + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, []byte(signing))), nil
}

// PublicKeyString is a key in the catalog's form: "ed25519:" + base64url.
func PublicKeyString(pub ed25519.PublicKey) string {
	return "ed25519:" + base64.RawURLEncoding.EncodeToString(pub)
}
