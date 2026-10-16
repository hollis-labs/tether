package devicepair

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/hollis-labs/tether/internal/environment"
)

// Renewal extends the existing bearer; the endpoint never issues a new token.
type Renewal struct {
	ExpiresAt time.Time `json:"expires_at"`
}

// Renew preserves current scopes by sending no requested scope restriction.
// Errors never include headers, response bodies, or the credential. Redirects
// are refused even on the same host so the bearer cannot leave this endpoint.
func Renew(ctx context.Context, client *http.Client, hub, token string) (Renewal, error) {
	return RenewWithHost(ctx, client, hub, token, "")
}

// RenewWithHost supports a loopback listener with an explicit allowed HTTP Host.
// host changes routing only; the HTTP connection still uses hub.
func RenewWithHost(ctx context.Context, client *http.Client, hub, token, host string) (Renewal, error) {
	var out Renewal
	u, err := url.Parse(hub)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return out, fmt.Errorf("renewal requires an HTTP(S) hub URL without credentials, query or fragment")
	}
	if token == "" || strings.ContainsAny(token, " \t\r\n") {
		return out, fmt.Errorf("invalid device credential")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(hub, "/")+"/auth/renew", strings.NewReader("{}"))
	if err != nil {
		return out, fmt.Errorf("invalid renewal request")
	}
	req.Host = host
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(environment.ProtocolHeader, "1")
	c := http.Client{Timeout: 15 * time.Second}
	if client != nil {
		c = *client
	}
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := c.Do(req) //nolint:gosec // G704: enrolled/operator-configured hub, redirects disabled.
	if err != nil {
		return out, fmt.Errorf("device renewal request failed")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return out, fmt.Errorf("device renewal refused (HTTP %d)", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4097))
	if err != nil || len(raw) > 4096 || json.Unmarshal(raw, &out) != nil || out.ExpiresAt.IsZero() || !out.ExpiresAt.After(time.Now()) {
		return Renewal{}, fmt.Errorf("invalid device renewal response")
	}
	return out, nil
}
