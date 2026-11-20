package runtoken

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestOfflineVerificationRejectsForgeryExpiryAndConfusion(t *testing.T) {
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	c := Claims{URN: "msg://agent/a", Session: "s", RunID: "r", JTI: "j", Expires: now.Add(time.Hour).Unix()}
	good, err := Sign(key, c)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Verify(pub, good, now)
	if err != nil || got.RunID != "r" || got.Task != "" {
		t.Fatal("valid token refused")
	}
	// Re-sign adversarial claims to distinguish semantic checks from signature checks.
	signed := func(c Claims, h string) string {
		b, _ := json.Marshal(c)
		p := encoding.EncodeToString([]byte(h)) + "." + encoding.EncodeToString(b)
		return p + "." + encoding.EncodeToString(ed25519.Sign(key, []byte(p)))
	}
	expired := got
	expired.Expires = now.Unix()
	wrongAud := got
	wrongAud.Audience = "tether"
	wrongIss := got
	wrongIss.Issuer = "other"
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	forged, _ := Sign(other, c)
	for name, token := range map[string]string{"forged": forged, "expired": signed(expired, header), "audience": signed(wrongAud, header), "issuer": signed(wrongIss, header), "type": signed(got, `{"alg":"EdDSA","typ":"JWT"}`), "algorithm": signed(got, `{"alg":"none","typ":"tether-run+jwt"}`), "malformed": "secret-malformed", "oversize": strings.Repeat("x", MaxSize+1)} {
		t.Run(name, func(t *testing.T) {
			if _, err := Verify(pub, token, now); !errors.Is(err, ErrInvalid) {
				t.Fatal("invalid token accepted or unsafe error")
			}
		})
	}
	if _, err := Verify(pub, good, now.Add(2*time.Hour)); !errors.Is(err, ErrInvalid) {
		t.Fatal("retained token outlived expiry")
	}
}
func TestHeaderPresence(t *testing.T) {
	for _, h := range []http.Header{{Header: {""}}, {Header: {"a", "b"}}, {Header: {strings.Repeat("x", MaxSize+1)}}} {
		if _, err := FromHeader(h); !errors.Is(err, ErrInvalid) {
			t.Fatal("ambiguous token accepted")
		}
	}
	if v, err := FromHeader(http.Header{}); err != nil || v != "" {
		t.Fatal("legacy missing token refused")
	}
}
