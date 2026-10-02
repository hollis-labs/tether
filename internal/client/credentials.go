package client

import (
	"errors"
	"fmt"
	"github.com/hollis-labs/tether/internal/identity"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
)

// WithToken supplies an opaque bearer credential. An explicit empty token
// disables environment/default-file lookup (e.g. for an anonymous health probe).
func WithToken(token string) Option {
	return func(c *Client) { c.token, c.tokenSet = token, true }
}

// WithTokenFile reads a current-user-owned regular 0600 file at construction.
// It takes precedence over WithToken and TETHER_TOKEN, independent of option
// order. Missing or insecure explicit files fail requests; clients never create one.
func WithTokenFile(path string) Option {
	return func(c *Client) { c.tokenFile, c.tokenFileSet = path, true }
}

func (c *Client) configureCredentials() error {
	token, err := c.resolveToken()
	if err != nil {
		return err
	}
	if token == "" {
		return nil
	}
	if !validBearerToken(token) {
		return fmt.Errorf("tether: invalid bearer credential")
	}
	origin, err := url.Parse(c.baseURL)
	if err != nil || origin.Host == "" || (origin.Scheme != "http" && origin.Scheme != "https") {
		return fmt.Errorf("tether: invalid base URL for authenticated client")
	}
	base := c.http.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	// Do not mutate a caller-owned client/transport or its redirect policy.
	cloned := *c.http
	cloned.Transport = &credentialTransport{base: base, token: token, scheme: origin.Scheme, host: origin.Host}
	c.http = &cloned
	return nil
}

func (c *Client) resolveToken() (string, error) {
	if c.tokenFileSet {
		if c.tokenFile == "" {
			return "", fmt.Errorf("tether: token file path required")
		}
		return identity.ReadTokenFile(c.tokenFile)
	}
	if c.tokenSet {
		return c.token, nil
	}
	if token := os.Getenv("TETHER_TOKEN"); token != "" {
		return token, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("tether: locate default credential: %w", err)
	}
	path := c.defaultTokenFile
	if path == "" {
		path = filepath.Join(home, ".tether", "run", "operator.token")
	}
	token, err := identity.ReadTokenFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	return token, err
}

func validBearerToken(token string) bool {
	if token == "" || len(token) > 256 {
		return false
	}
	for _, b := range []byte(token) {
		if b < 33 || b > 126 {
			return false
		}
	}
	return true
}

type Option func(*Client)

type credentialTransport struct {
	base                http.RoundTripper
	err                 error
	token, scheme, host string
}

func (t *credentialTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.err != nil {
		return nil, t.err
	}
	if t.token == "" {
		return t.base.RoundTrip(req)
	}
	// A client redirect can invoke RoundTrip again at another origin. Refuse
	// before sending either credential or request there, including redirects
	// that net/http would normally trust (such as a parent/subdomain change).
	if req.URL.Scheme != t.scheme || req.URL.Host != t.host {
		return nil, fmt.Errorf("tether: authenticated request changed origin")
	}
	cloned := req.Clone(req.Context())
	cloned.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(cloned)
}

func (t *credentialTransport) CloseIdleConnections() {
	if closer, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

// WithTokenFileDefault changes the optional operator credential location.
func WithTokenFileDefault(path string) Option { return func(c *Client) { c.defaultTokenFile = path } }

// credentialError keeps file errors distinct from socket/dial failures, so
// callers cannot turn a bad credential into a local-state fallback.
type credentialError struct{ cause error }

func (e *credentialError) Error() string { return e.cause.Error() }
func (e *credentialError) Unwrap() error { return e.cause }
