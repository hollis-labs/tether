package enrollment

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"testing"
	"time"

	"filippo.io/edwards25519"
)

func TestWeakSigningKeysCannotProvePossession(t *testing.T) {
	keys, _, _, binding, now := fixture(t)
	// This is the reported forgery: identity R, identity A, S=0 verifies
	// under crypto/ed25519 for any message without a signing private key.
	identity := edwards25519.NewIdentityPoint().Bytes()
	forged := make([]byte, ed25519.SignatureSize)
	copy(forged, identity)
	keys.Signing = base64.RawURLEncoding.EncodeToString(identity)
	binding.SigningThumbprint = thumbprint("Ed25519", keys.Signing)
	c := Challenge{Profile: PossessionProfile, Binding: binding, Action: "redeem", Nonce: base64.RawURLEncoding.EncodeToString(make([]byte, 32)), IssuedAt: now.Unix(), ExpiresAt: now.Add(time.Minute).Unix()}
	if !ed25519.Verify(identity, challengeInfo(c), forged) {
		t.Fatal("reported weak-key forgery was not reproduced")
	}
	if Verify(keys, c, base64.RawURLEncoding.EncodeToString(forged), now) == nil {
		t.Fatal("weak-key forgery accepted by custody verifier")
	}
	// An order-eight point generates the complete torsion subgroup. Also
	// exercise every nontrivial torsion component added to a valid public key.
	encoded, _ := hex.DecodeString("c7176a703d4dd84fba3c0b760d10670f2a2053fa2c39ccc64ec7fd7792ac037a")
	g, err := new(edwards25519.Point).SetBytes(encoded)
	if err != nil {
		t.Fatal(err)
	}
	p := edwards25519.NewIdentityPoint()
	for i := 0; i < 8; i++ {
		keys.Signing = base64.RawURLEncoding.EncodeToString(p.Bytes())
		if _, _, err := PublicKeys(keys); err == nil {
			t.Fatalf("torsion key %d accepted", i)
		}
		if i > 0 {
			mixed := new(edwards25519.Point).Add(edwards25519.NewGeneratorPoint(), p)
			keys.Signing = base64.RawURLEncoding.EncodeToString(mixed.Bytes())
			if _, _, err := PublicKeys(keys); err == nil {
				t.Fatalf("mixed-order key %d accepted", i)
			}
		}
		p.Add(p, g)
	}
	if p.Equal(edwards25519.NewIdentityPoint()) != 1 {
		t.Fatal("invalid order-eight fixture")
	}
	// Noncanonical y=p..p+18, both sign bits; plus negative zero encodings.
	for y := 0xed; y <= 0xff; y++ {
		for _, sign := range []byte{0, 0x80} {
			raw := bytes.Repeat([]byte{0xff}, 32)
			raw[0], raw[31] = byte(y), 0x7f|sign
			if strongSigningKey(raw) {
				t.Fatal("noncanonical point accepted")
			}
		}
	}
	identity[31] |= 0x80
	if strongSigningKey(identity) || strongSigningKey([]byte{1}) {
		t.Fatal("invalid point encoding accepted")
	}
}

func TestRFC8037Thumbprint(t *testing.T) {
	keys, _, _, _, _ := fixture(t)
	keys.Signing = "11qYAYKxCrfVS_7TyWQHOg7hcvPapiMlrwIaaPcHURo"
	got, _, err := PublicKeys(keys)
	if err != nil || got != "kPrK_qmxVWaYVA9wwBF6Iuo3vVzz7TxHCTwXBygrS4k" {
		t.Fatal("RFC8037 A.3 public-key thumbprint mismatch")
	}
}

func TestSignedDeliveryRejectsPublicKeySubstitution(t *testing.T) {
	keys, _, enc, binding, now := fixture(t)
	pub, signer, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	trust := DeliveryTrust{Issuer: binding.Issuer, Keys: map[string]ed25519.PublicKey{"issuer-key": pub}, KeysCheckedAt: now, EmergencyCheckedAt: now, Enabled: true}
	signed, err := sealSigned(signer, "issuer-key", keys, binding, 17, []byte("synthetic"), now, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	got, err := OpenSigned(trust, keys, enc, binding, 17, signed, now)
	if err != nil || string(got) != "synthetic" {
		t.Fatal("signed delivery failed")
	}
	// Anyone can seal attacker-chosen plaintext to the destination public key.
	// The issuer signature must reject that substitution before opening it.
	forged, err := seal(keys, binding, 17, []byte("attacker-chosen"), now, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	mutations := map[string]func(*SignedDelivery){
		"ciphertext": func(d *SignedDelivery) { d.Delivery = forged },
		"signature":  func(d *SignedDelivery) { d.Signature = "" },
		"profile":    func(d *SignedDelivery) { d.Profile = "other" },
		"kid":        func(d *SignedDelivery) { d.KeyID = "other" },
		"issuer":     func(d *SignedDelivery) { d.Delivery.Binding.Issuer = "other" },
		"credential": func(d *SignedDelivery) { d.Delivery.CredentialID++ },
		"expiry":     func(d *SignedDelivery) { d.Delivery.ExpiresAt-- },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			d := signed
			mutate(&d)
			if _, err := OpenSigned(trust, keys, enc, binding, 17, d, now); err == nil {
				t.Fatal("tampered delivery accepted")
			}
		})
	}
	policies := map[string]func(*DeliveryTrust){
		"disabled":        func(p *DeliveryTrust) { p.Enabled = false },
		"denied":          func(p *DeliveryTrust) { p.DeniedKeyIDs = map[string]bool{"issuer-key": true} },
		"stale keys":      func(p *DeliveryTrust) { p.KeysCheckedAt = now.Add(-time.Minute) },
		"stale control":   func(p *DeliveryTrust) { p.EmergencyCheckedAt = now.Add(-5 * time.Second) },
		"unknown keys":    func(p *DeliveryTrust) { p.Keys = nil },
		"future snapshot": func(p *DeliveryTrust) { p.KeysCheckedAt = now.Add(time.Second) },
		"other issuer":    func(p *DeliveryTrust) { p.Issuer = "other" },
		"weak issuer key": func(p *DeliveryTrust) {
			p.Keys = map[string]ed25519.PublicKey{"issuer-key": edwards25519.NewIdentityPoint().Bytes()}
		},
	}
	for name, mutate := range policies {
		t.Run(name, func(t *testing.T) {
			p := trust
			mutate(&p)
			if _, err := OpenSigned(p, keys, enc, binding, 17, signed, now); err == nil {
				t.Fatal("untrusted delivery accepted")
			}
		})
	}
}

func TestDestinationSkewIssuerBindingAndIDHygiene(t *testing.T) {
	keys, sign, enc, binding, now := fixture(t)
	c, err := NewChallenge(keys, binding, "redeem", 0, now)
	if err != nil {
		t.Fatal(err)
	}
	d, err := seal(keys, binding, 17, []byte("synthetic"), now, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	for _, delta := range []time.Duration{-5 * time.Second, -6 * time.Second, time.Minute} {
		_, signErr := Sign(sign, keys, binding, c, now.Add(delta))
		_, openErr := open(keys, enc, binding, d, now.Add(delta))
		wantOK := delta == -5*time.Second
		if (signErr == nil) != wantOK || (openErr == nil) != wantOK {
			t.Fatal("destination skew boundary")
		}
	}
	sig, err := Sign(sign, keys, binding, c, now)
	if err != nil {
		t.Fatal(err)
	}
	if Verify(keys, c, sig, now.Add(-time.Second)) == nil {
		t.Fatal("server allowed future issuance")
	}
	c.Binding.Issuer = "other-authority"
	if Verify(keys, c, sig, now) == nil {
		t.Fatal("cross-authority proof accepted")
	}
	d.Binding.Issuer = c.Binding.Issuer
	if _, err := open(keys, enc, d.Binding, d, now); err == nil {
		t.Fatal("cross-authority ciphertext opened")
	}
	for _, id := range []string{"a\u202eb", "a\u200db", "a\ue000b", "café", "a b", "a\n"} {
		bad := binding
		bad.DestinationID = id
		if _, err := NewChallenge(keys, bad, "redeem", 0, now); err == nil {
			t.Fatal("unsafe display ID accepted")
		}
	}
}
