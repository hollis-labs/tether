package client

import (
	"context"
	"net/http"
	"net/http/httptest"
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

// WithTokenSource: the bearer is read per request, so a credential replaced while the client lives is the one sent.
func TestTokenSourceIsReadPerRequest(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	current := "first-token"
	c := New("tcp:"+srv.URL[len("http://"):], WithTokenSource(func() string { return current }))
	for _, tok := range []string{"first-token", "second-token", strings.Repeat("j", 470)} {
		current = tok
		if err := c.Ping(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(seen) != 3 || seen[0] != "Bearer first-token" || seen[1] != "Bearer second-token" || seen[2] != "Bearer "+strings.Repeat("j", 470) {
		t.Fatalf("Authorization headers = %q", seen)
	}
	current = "bad token"
	if err := c.Ping(context.Background()); err == nil {
		t.Fatal("a malformed token from the source was sent")
	}
	if len(seen) != 3 {
		t.Fatal("a request with a malformed token reached the server")
	}
}
