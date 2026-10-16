package client

import (
	"context"
	"errors"
	"fmt"
	"github.com/hollis-labs/tether/internal/environment"
	"github.com/hollis-labs/tether/internal/identity"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
)

// VerifiedPrincipal proves which principal this client's credential will send.
// Daemon MCP views use it to prevent a native API client from carrying broader
// operator authority than the admitted caller. The bearer stays private.
func (c *Client) VerifiedPrincipal(ctx context.Context, verifier identity.Verifier) (identity.Principal, error) {
	if c == nil || c.http == nil || verifier == nil {
		return identity.Principal{}, identity.ErrInvalidToken
	}
	transport, ok := c.http.Transport.(*credentialTransport)
	if !ok || transport.err != nil {
		return identity.Principal{}, identity.ErrInvalidToken
	}
	token := transport.current()
	if token == "" || !validBearerToken(token) {
		return identity.Principal{}, identity.ErrInvalidToken
	}
	return verifier.Verify(ctx, token)
}

// WithToken supplies an opaque bearer credential. An explicit empty token
// disables environment/default-file lookup (e.g. for an anonymous health probe).
func WithToken(token string) Option {
	return func(c *Client) { c.token, c.tokenSet = token, true }
}

// WithTokenSource supplies the bearer for each request from fn instead of a fixed string, for a caller whose
// credential is replaced while the client lives (an issued agent token the outpost rewrites every few minutes).
// fn is called per request and must return the current token; an empty or malformed value fails the request
// before anything is sent. It takes precedence over WithToken, TETHER_TOKEN and the default token file.
func WithTokenSource(fn func() string) Option {
	return func(c *Client) { c.tokenSource = fn }
}

// WithProtocolHeader makes every request carry the environment protocol
// version. A remote listener requires it (environment.ProtocolGate with
// require=true) and answers 409 protocol_mismatch without it. Local unix
// callers may omit it, so only remote clients (--target) opt in.
func WithProtocolHeader() Option {
	return func(c *Client) { c.protocolHeader = true }
}

// WithTokenFile reads a current-user-owned regular 0600 file at construction.
// It takes precedence over WithToken and TETHER_TOKEN, independent of option
// order. Missing or insecure explicit files fail requests; clients never create one.
func WithTokenFile(path string) Option {
	return func(c *Client) { c.tokenFile, c.tokenFileSet = path, true }
}

func (c *Client) configureCredentials() error {
	var token string
	if c.tokenSource != nil {
		// Validated per request; a placeholder keeps the transport in authenticated mode.
		token = c.tokenSource()
		if !validBearerToken(token) {
			return fmt.Errorf("tether: invalid bearer credential")
		}
	} else {
		var err error
		token, err = c.resolveToken()
		if err != nil {
			return err
		}
		if token == "" {
			return nil
		}
		if !validBearerToken(token) {
			return fmt.Errorf("tether: invalid bearer credential")
		}
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
	cloned.Transport = &credentialTransport{base: base, token: token, source: c.tokenSource, scheme: origin.Scheme, host: origin.Host, protocol: c.protocolHeader}
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

// maxBearerToken bounds a bearer credential. Stored credentials are short; an issued agent JWT is about
// 470 characters, so the old 256 cap made the daemon's native client refuse every issued token.
const maxBearerToken = 4096

func validBearerToken(token string) bool {
	if token == "" || len(token) > maxBearerToken {
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
	// source, when set, replaces token on every request (WithTokenSource).
	source   func() string
	protocol bool
}

// current is the bearer this transport would send now.
func (t *credentialTransport) current() string {
	if t.source != nil {
		return t.source()
	}
	return t.token
}

func (t *credentialTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.err != nil {
		return nil, t.err
	}
	token := t.current()
	if token == "" && t.source == nil {
		return t.base.RoundTrip(req)
	}
	if t.source != nil && !validBearerToken(token) {
		return nil, fmt.Errorf("tether: invalid bearer credential")
	}
	// A client redirect can invoke RoundTrip again at another origin. Refuse
	// before sending either credential or request there, including redirects
	// that net/http would normally trust (such as a parent/subdomain change).
	if req.URL.Scheme != t.scheme || req.URL.Host != t.host {
		return nil, fmt.Errorf("tether: authenticated request changed origin")
	}
	cloned := req.Clone(req.Context())
	cloned.Header.Set("Authorization", "Bearer "+token)
	if t.protocol {
		cloned.Header.Set(environment.ProtocolHeader, strconv.Itoa(environment.Protocol))
	}
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
