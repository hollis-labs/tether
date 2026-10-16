package devicepair

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRenewPreservesBearerAndScopes(t *testing.T) {
	token := "tth_synthetic_renewal"
	expiry := time.Now().Add(30 * 24 * time.Hour).UTC().Truncate(time.Second)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if r.Method != "POST" || r.URL.Path != "/auth/renew" || r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get("Tether-Protocol") != "1" || string(b) != "{}" {
			t.Error("renewal request did not preserve credential/scopes")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"expires_at": expiry, "id": "device", "scopes": []string{"worker"}})
	}))
	defer server.Close()
	got, err := Renew(context.Background(), server.Client(), server.URL, token)
	if err != nil || !got.ExpiresAt.Equal(expiry) {
		t.Fatalf("renewal %v %v", got, err)
	}
}
func TestRenewRefusesRedirectAndRedactsErrors(t *testing.T) {
	token := "tth_synthetic_never_print"
	hits := 0
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++; w.WriteHeader(200) }))
	defer dest.Close()
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusUnauthorized, http.StatusForbidden, http.StatusOK} {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", dest.URL)
			w.WriteHeader(status)
			_, _ = io.WriteString(w, token)
		}))
		_, err := Renew(context.Background(), s.Client(), s.URL, token)
		s.Close()
		if err == nil || strings.Contains(err.Error(), token) {
			t.Fatalf("unsafe error: %v", err)
		}
	}
	if hits != 0 {
		t.Fatal("followed redirect with credential")
	}
}
func TestRenewCancellationAndBadURL(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, hub := range []string{"http://127.0.0.1:1", "https://user:secret@example.com", "https://example.com?token=secret", "file:///tmp/socket"} {
		_, err := Renew(ctx, nil, hub, "tth_synthetic")
		if err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("unsafe result: %v", err)
		}
	}
}

// A fake transport proves refusal happens before a request can carry a bearer,
// independently of DNS/network availability. The Host override cannot bypass it.
type renewalTransport func(*http.Request) (*http.Response, error)

func (f renewalTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestRenewRejectsRemoteHTTPBeforeSending(t *testing.T) {
	for _, hub := range []string{"http://hub.example", "http://192.0.2.10", "http://[2001:db8::1]", "http://127.0.0.1.example", "http://localhost.example"} {
		client := &http.Client{Transport: renewalTransport(func(*http.Request) (*http.Response, error) {
			t.Error("sent remote cleartext credential")
			return nil, fmt.Errorf("unexpected request")
		})}
		_, err := RenewWithHost(context.Background(), client, hub, "tth_synthetic", "localhost")
		if err == nil || !strings.Contains(err.Error(), "requires HTTPS") {
			t.Fatalf("hub %s: %v", hub, err)
		}
	}
}
func TestRenewAcceptsHTTPSAndLoopbackHTTP(t *testing.T) {
	for _, hub := range []string{"https://hub.example", "http://localhost", "http://127.0.0.2", "http://[::1]"} {
		calls := 0
		client := &http.Client{Transport: renewalTransport(func(*http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(fmt.Sprintf(`{"expires_at":%q}`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))))}, nil
		})}
		if _, err := Renew(context.Background(), client, hub, "tth_synthetic"); err != nil || calls != 1 {
			t.Fatalf("hub %s calls%d error%v", hub, calls, err)
		}
	}
}
