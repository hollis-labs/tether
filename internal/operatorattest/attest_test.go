package operatorattest

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"
)

func newKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func TestParseValidatesTheTrustRoot(t *testing.T) {
	pub, _ := newKey(t)
	good := RawKey{ID: "tangent-2026-11", PublicKey: PublicKeyString(pub)}
	if c, err := Parse(RawConfig{Issuer: "tangent", MaxAgeSeconds: 300, Keys: []RawKey{good}}); err != nil || c.MaxAge != 300*time.Second || len(c.Keys) != 1 {
		t.Fatalf("valid block: %+v %v", c, err)
	}
	for name, raw := range map[string]RawConfig{
		"no issuer":   {MaxAgeSeconds: 300, Keys: []RawKey{good}},
		"max age 301": {Issuer: "tangent", MaxAgeSeconds: 301, Keys: []RawKey{good}},
		"max age 0":   {Issuer: "tangent", Keys: []RawKey{good}},
		"no keys":     {Issuer: "tangent", MaxAgeSeconds: 300},
		"three keys":  {Issuer: "tangent", MaxAgeSeconds: 300, Keys: []RawKey{good, {ID: "b", PublicKey: good.PublicKey}, {ID: "c", PublicKey: good.PublicKey}}},
		"repeated id": {Issuer: "tangent", MaxAgeSeconds: 300, Keys: []RawKey{good, good}},
		"no prefix":   {Issuer: "tangent", MaxAgeSeconds: 300, Keys: []RawKey{{ID: "a", PublicKey: strings.TrimPrefix(good.PublicKey, "ed25519:")}}},
		"short key":   {Issuer: "tangent", MaxAgeSeconds: 300, Keys: []RawKey{{ID: "a", PublicKey: "ed25519:" + base64.RawURLEncoding.EncodeToString(make([]byte, 31))}}},
	} {
		if _, err := Parse(raw); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// fixture is a signed, fully bound assertion and what it is bound to.
type fixture struct {
	cfg    Config
	priv   ed25519.PrivateKey
	claims Claims
	expect Expect
	now    time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pub, priv := newKey(t)
	now := time.Unix(1_800_000_000, 0)
	note := "ship it"
	noteHash := SHA256Hex(note)
	nonce := "bm9uY2Utbm9uY2Utbm9uY2Utbm9uY2Utbm9uY2U"
	f := &fixture{cfg: Config{Issuer: "tangent", MaxAge: 300 * time.Second, Keys: []Key{{ID: "k1", Public: pub}}}, priv: priv, now: now}
	f.claims = Claims{Iss: "tangent", Aud: Audience, Sub: Subject, Jti: "jti-1", Iat: now.Unix() - 10, Exp: now.Unix() + 200,
		ChallengeID: "orc_1", Nonce: nonce, MessageID: "m1", Option: "approve", Binding: Binding("m1", "approve"),
		BodySHA256: SHA256Hex("body"), NoteSHA256: &noteHash, DeviceID: "msg://device/tachyon", ElevationID: "el-1", ElevatedAt: now.Unix() - 20}
	f.expect = Expect{Issuer: "tangent", ChallengeID: "orc_1", NonceSHA256: SHA256Hex(nonce), MessageID: "m1", Option: "approve",
		Binding: Binding("m1", "approve"), BodySHA256: SHA256Hex("body"), DeviceID: "msg://device/tachyon", Note: &note}
	return f
}

func (f *fixture) check(t *testing.T) error {
	t.Helper()
	token, err := Sign(f.claims, "k1", f.priv)
	if err != nil {
		t.Fatal(err)
	}
	claims, kid, err := Verify(token, f.cfg)
	if err != nil {
		return err
	}
	if kid != "k1" {
		t.Fatalf("kid %q", kid)
	}
	if err = CheckFresh(claims, f.now, f.cfg.MaxAge); err != nil {
		return err
	}
	return CheckBinding(claims, f.expect)
}

func TestAValidAssertionPasses(t *testing.T) {
	if err := newFixture(t).check(t); err != nil {
		t.Fatal(err)
	}
}

func TestSignatureAndFormFailuresAreInvalid(t *testing.T) {
	f := newFixture(t)
	token, _ := Sign(f.claims, "k1", f.priv)
	parts := strings.Split(token, ".")
	other, otherPriv := newKey(t)
	_ = other
	forged, _ := Sign(f.claims, "k1", otherPriv)
	unknown, _ := Sign(f.claims, "k9", f.priv)
	hs := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","kid":"k1","typ":"operator-reply+jwt"}`))
	none := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","kid":"k1","typ":"operator-reply+jwt"}`))
	badTyp := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"EdDSA","kid":"k1","typ":"JWT"}`))
	for name, tok := range map[string]string{
		"two parts":        parts[0] + "." + parts[1],
		"forged signature": forged,
		"unknown kid":      unknown,
		"tampered payload": parts[0] + "." + base64.RawURLEncoding.EncodeToString([]byte(`{"option":"deny"}`)) + "." + parts[2],
		"alg HS256":        hs + "." + parts[1] + "." + parts[2],
		"alg none":         none + "." + parts[1] + ".",
		"wrong typ":        badTyp + "." + parts[1] + "." + parts[2],
		"garbage":          "a.b.c",
	} {
		if _, _, err := Verify(tok, f.cfg); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v, want assertion_invalid", name, err)
		}
	}
}

// The freshness bound is Tether's own: a 6-minute-old step-up is stale even if
// the operator's elevation_ttl is "always".
func TestFreshnessRules(t *testing.T) {
	for name, mutate := range map[string]func(*Claims, time.Time){
		"expired":                      func(c *Claims, n time.Time) { c.Exp = n.Unix() },
		"lifetime over max age":        func(c *Claims, n time.Time) { c.Iat = n.Unix() - 10; c.Exp = c.Iat + 301; c.ElevatedAt = c.Iat },
		"iat in the future":            func(c *Claims, n time.Time) { c.Iat = n.Unix() + 31; c.Exp = c.Iat + 100; c.ElevatedAt = c.Iat },
		"iat too old":                  func(c *Claims, n time.Time) { c.Iat = n.Unix() - 301; c.Exp = n.Unix() + 1 },
		"six-minute-old step-up":       func(c *Claims, n time.Time) { c.ElevatedAt = n.Unix() - 360 },
		"elevated_at in the future":    func(c *Claims, n time.Time) { c.ElevatedAt = n.Unix() + 31 },
		"elevated after the assertion": func(c *Claims, n time.Time) { c.ElevatedAt = c.Iat + 1 },
	} {
		f := newFixture(t)
		mutate(&f.claims, f.now)
		if err := f.check(t); !errors.Is(err, ErrStale) {
			t.Errorf("%s: %v, want assertion_stale", name, err)
		}
	}
	// Within the skew is fine.
	f := newFixture(t)
	f.claims.Iat, f.claims.ElevatedAt = f.now.Unix()+29, f.now.Unix()+29
	f.claims.Exp = f.claims.Iat + 100
	if err := f.check(t); err != nil {
		t.Fatalf("within skew: %v", err)
	}
}

func TestBindingRules(t *testing.T) {
	for name, mutate := range map[string]func(*fixture){
		"iss":                    func(f *fixture) { f.claims.Iss = "other" },
		"aud":                    func(f *fixture) { f.claims.Aud = "tangent" },
		"sub":                    func(f *fixture) { f.claims.Sub = "msg://agent/x" },
		"no jti":                 func(f *fixture) { f.claims.Jti = "" },
		"no elevation_id":        func(f *fixture) { f.claims.ElevationID = "" },
		"challenge":              func(f *fixture) { f.claims.ChallengeID = "orc_2" },
		"nonce":                  func(f *fixture) { f.claims.Nonce = "other" },
		"message":                func(f *fixture) { f.claims.MessageID = "m2"; f.claims.Binding = Binding("m2", "approve") },
		"option":                 func(f *fixture) { f.claims.Option = "deny"; f.claims.Binding = Binding("m1", "deny") },
		"binding":                func(f *fixture) { f.claims.Binding = Binding("m1", "deny") },
		"body":                   func(f *fixture) { f.claims.BodySHA256 = SHA256Hex("other") },
		"device":                 func(f *fixture) { f.claims.DeviceID = "msg://device/other" },
		"note without hash":      func(f *fixture) { f.claims.NoteSHA256 = nil },
		"hash without note":      func(f *fixture) { f.expect.Note = nil },
		"different note":         func(f *fixture) { other := "deny it"; f.expect.Note = &other },
		"request option differs": func(f *fixture) { f.expect.Option = "deny"; f.expect.Binding = Binding("m1", "deny") },
	} {
		f := newFixture(t)
		mutate(f)
		if err := f.check(t); !errors.Is(err, ErrUnbound) {
			t.Errorf("%s: %v, want assertion_unbound", name, err)
		}
	}
}
