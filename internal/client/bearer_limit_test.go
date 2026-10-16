package client

import (
	"strings"
	"testing"
)

// An issued agent JWT is about 470 characters; the daemon's native client must be able to carry one.
func TestValidBearerTokenAcceptsIssuedJWTLength(t *testing.T) {
	jwt := strings.Repeat("a", 150) + "." + strings.Repeat("b", 150) + "." + strings.Repeat("c", 170)
	if !validBearerToken(jwt) {
		t.Fatalf("a %d-character JWT was refused", len(jwt))
	}
	for name, tok := range map[string]string{"empty": "", "too long": strings.Repeat("a", maxBearerToken+1), "space": "tok en", "control": "tok\x01en"} {
		if validBearerToken(tok) {
			t.Errorf("%s token accepted", name)
		}
	}
}
