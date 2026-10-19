package enrollment

import (
	"bytes"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T) (DestinationKeys, ed25519.PrivateKey, *ecdh.PrivateKey, Binding, time.Time) {
	t.Helper()
	pub, sign, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keys := DestinationKeys{Signing: base64.RawURLEncoding.EncodeToString(pub), Encryption: base64.RawURLEncoding.EncodeToString(enc.PublicKey().Bytes())}
	st, et, err := PublicKeys(keys)
	if err != nil {
		t.Fatal(err)
	}
	b := Binding{Issuer: "tether", GrantID: "grant-1", OperationDigest: "sha256:" + strings.Repeat("a", 64), PrincipalID: "producer-1", DestinationID: "agent-os", DestinationRevision: "1", SigningThumbprint: st, EncryptionThumbprint: et}
	return keys, sign, enc, b, time.Unix(1800000000, 0)
}

func TestSealedDeliveryRoundTripAndRetainedRetry(t *testing.T) {
	keys, _, private, binding, now := fixture(t)
	credential := []byte("synthetic-private-credential-not-for-use")
	d, err := seal(keys, binding, 17, credential, now, now.Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, credential) {
		t.Fatal("delivery retained plaintext")
	}
	var restored Delivery
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		got, err := open(keys, private, binding, restored, now.Add(time.Second))
		if err != nil || !bytes.Equal(got, credential) {
			t.Fatal("retained envelope failed to open")
		}
	}
	_, _, wrong, _, _ := fixture(t)
	if _, err := open(keys, wrong, binding, d, now); err == nil {
		t.Fatal("other destination opened credential")
	}
}

func TestSealedDeliveryBindsEveryField(t *testing.T) {
	keys, _, private, binding, now := fixture(t)
	d, err := seal(keys, binding, 17, []byte("synthetic"), now, now.Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	mutations := map[string]func(*Delivery){
		"profile":     func(d *Delivery) { d.Profile = "other" },
		"grant":       func(d *Delivery) { d.Binding.GrantID = "other" },
		"operation":   func(d *Delivery) { d.Binding.OperationDigest = "sha256:" + strings.Repeat("b", 64) },
		"principal":   func(d *Delivery) { d.Binding.PrincipalID = "other" },
		"destination": func(d *Delivery) { d.Binding.DestinationID = "other" },
		"revision":    func(d *Delivery) { d.Binding.DestinationRevision = "2" },
		"signing key": func(d *Delivery) {
			d.Binding.SigningThumbprint = base64.RawURLEncoding.EncodeToString(make([]byte, 32))
		},
		"encryption key": func(d *Delivery) {
			d.Binding.EncryptionThumbprint = base64.RawURLEncoding.EncodeToString(make([]byte, 32))
		},
		"credential": func(d *Delivery) { d.CredentialID++ },
		"issued":     func(d *Delivery) { d.IssuedAt-- },
		"expires":    func(d *Delivery) { d.ExpiresAt-- },
		"corrupt ciphertext": func(d *Delivery) {
			b, _ := base64.RawURLEncoding.DecodeString(d.Ciphertext)
			b[len(b)-1] ^= 1
			d.Ciphertext = base64.RawURLEncoding.EncodeToString(b)
		},
		"padded ciphertext":    func(d *Delivery) { d.Ciphertext += "=" },
		"oversized ciphertext": func(d *Delivery) { d.Ciphertext = strings.Repeat("a", MaxCredentialBytes*2) },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			changed := d
			mutate(&changed)
			// Even an attacker changing the expected metadata cannot decrypt the
			// ciphertext under another binding; the HPKE info binds it too.
			if _, err := open(keys, private, changed.Binding, changed, now); err == nil {
				t.Fatal("mutation opened")
			}
		})
	}
	for _, at := range []time.Time{now.Add(-6 * time.Second), time.Unix(d.ExpiresAt, 0)} {
		if _, err := open(keys, private, binding, d, at); err == nil {
			t.Fatal("time boundary accepted")
		}
	}
}

func TestPossessionBindsServerChallengeAndAction(t *testing.T) {
	keys, private, _, binding, now := fixture(t)
	c, err := NewChallenge(keys, binding, "redeem", 0, now)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := Sign(private, keys, binding, c, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify(keys, c, sig, now); err != nil {
		t.Fatal(err)
	}
	other, err := NewChallenge(keys, binding, "redeem", 0, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify(keys, other, sig, now); err == nil {
		t.Fatal("signature reused for a fresh nonce")
	}
	mutations := map[string]func(*Challenge){
		"grant":          func(c *Challenge) { c.Binding.GrantID = "other" },
		"operation":      func(c *Challenge) { c.Binding.OperationDigest = "sha256:" + strings.Repeat("b", 64) },
		"principal":      func(c *Challenge) { c.Binding.PrincipalID = "other" },
		"destination":    func(c *Challenge) { c.Binding.DestinationID = "other" },
		"revision":       func(c *Challenge) { c.Binding.DestinationRevision = "2" },
		"signing key":    func(c *Challenge) { c.Binding.SigningThumbprint = "other" },
		"encryption key": func(c *Challenge) { c.Binding.EncryptionThumbprint = "other" },
		"profile":        func(c *Challenge) { c.Profile = DeliveryProfile },
		"confirm":        func(c *Challenge) { c.Action = "confirm"; c.CredentialID = 17 },
		"invalid action": func(c *Challenge) { c.Action = "approve" },
		"credential":     func(c *Challenge) { c.CredentialID = 17 },
		"issued":         func(c *Challenge) { c.IssuedAt-- },
		"expires":        func(c *Challenge) { c.ExpiresAt-- },
		"nonce padding":  func(c *Challenge) { c.Nonce += "=" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			changed := c
			mutate(&changed)
			if Verify(keys, changed, sig, now) == nil {
				t.Fatal("changed challenge accepted")
			}
		})
	}
	for _, at := range []time.Time{now.Add(-time.Second), time.Unix(c.ExpiresAt, 0)} {
		if Verify(keys, c, sig, at) == nil {
			t.Fatal("expired or future challenge accepted")
		}
	}
	if Verify(keys, c, sig+"=", now) == nil {
		t.Fatal("noncanonical signature accepted")
	}
	changed := c
	changed.Binding.GrantID = "other"
	if _, err := Sign(private, keys, binding, changed, now); err == nil {
		t.Fatal("destination signed unexpected binding")
	}
	confirm, err := NewChallenge(keys, binding, "confirm", 17, now)
	if err != nil {
		t.Fatal(err)
	}
	confirmation, err := Sign(private, keys, binding, confirm, now)
	if err != nil || Verify(keys, confirm, confirmation, now) != nil {
		t.Fatal("confirmation failed")
	}
	if Verify(keys, c, confirmation, now) == nil {
		t.Fatal("confirmation authorized redemption")
	}
}

func TestCustodyRejectsMalformedKeysAndBounds(t *testing.T) {
	keys, private, enc, binding, now := fixture(t)
	for _, bad := range []DestinationKeys{{Signing: keys.Signing + "=", Encryption: keys.Encryption}, {Signing: keys.Signing, Encryption: base64.RawURLEncoding.EncodeToString(make([]byte, 32))}, {Signing: base64.RawURLEncoding.EncodeToString(make([]byte, 32)), Encryption: keys.Encryption}} {
		if _, _, err := PublicKeys(bad); err == nil {
			t.Fatal("invalid key accepted")
		}
	}
	for _, credential := range [][]byte{nil, make([]byte, MaxCredentialBytes+1)} {
		if _, err := seal(keys, binding, 1, credential, now, now.Add(time.Minute)); err == nil {
			t.Fatal("credential size accepted")
		}
	}
	for _, expiry := range []time.Time{now, now.Add(MaxDeliveryLifetime + time.Second)} {
		if _, err := seal(keys, binding, 1, []byte("synthetic"), now, expiry); err == nil {
			t.Fatal("delivery window accepted")
		}
	}
	bad := binding
	bad.DestinationRevision = "01"
	if _, err := seal(keys, bad, 1, []byte("synthetic"), now, now.Add(time.Minute)); err == nil {
		t.Fatal("noncanonical revision accepted")
	}
	bad = binding
	bad.DestinationID = "bad\x00id"
	if _, err := seal(keys, bad, 1, []byte("synthetic"), now, now.Add(time.Minute)); err == nil {
		t.Fatal("control byte accepted")
	}
	c, err := NewChallenge(keys, binding, "redeem", 0, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Sign(private[:1], keys, binding, c, now); err == nil {
		t.Fatal("short signing key accepted")
	}
	d, err := seal(keys, binding, 1, []byte("synthetic"), now, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := open(keys, nil, binding, d, now); err == nil {
		t.Fatal("nil encryption key accepted")
	}
	if _, err := open(keys, enc, bad, d, now); err == nil {
		t.Fatal("unexpected destination accepted")
	}
}
