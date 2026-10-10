package federation

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/hollis-labs/tether/internal/environment"
	"github.com/hollis-labs/tether/internal/identity"
)

var (
	ErrPeerCredential     = errors.New("federation: peer credential unavailable or invalid")
	ErrPeerAuthentication = errors.New("federation: peer refused authentication")
	ErrPeerScope          = errors.New("federation: peer refused authorization")
	ErrPeerOrigin         = errors.New("federation: authenticated request changed origin")
)

// PeerAuthError distinguishes credential resolution, authentication and scope
// refusal without returning token bytes, peer response bodies or private paths.
// A 401 does not distinguish a bad token from expiry or revocation.
type PeerAuthError struct {
	Authority  string
	StatusCode int
	Reason     error
}

func (e *PeerAuthError) Error() string {
	return fmt.Sprintf("federation: peer %q: %v (HTTP %d)", e.Authority, e.Reason, e.StatusCode)
}

func (e *PeerAuthError) Unwrap() error { return e.Reason }

// credentialPath accepts only an explicit local file reference. No default
// operator token, environment interpolation, helper invocation or file read is
// part of config validation or peer construction.
func credentialPath(ref string) (string, error) {
	u, err := url.Parse(ref)
	if err != nil || u.Scheme != "file" || u.Host != "" || u.User != nil ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" ||
		!strings.HasPrefix(ref, "file:///") || !filepath.IsAbs(u.Path) || u.Path == "/" ||
		strings.ContainsAny(u.Path, "\x00\r\n") {
		return "", ErrPeerCredential
	}
	return u.Path, nil
}

func validatePeerCredential(base *url.URL, ref string) error {
	if ref == "" {
		return nil
	}
	if _, err := credentialPath(ref); err != nil {
		return err
	}
	ip := net.ParseIP(base.Hostname())
	port := base.Port()
	if port == "" {
		port = "80"
	}
	n, portErr := strconv.Atoi(port)
	if base.Scheme != "http" || base.User != nil || base.RawQuery != "" || base.ForceQuery || base.Fragment != "" ||
		portErr != nil || n <= 0 || n > 65535 ||
		(base.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback())) {
		return fmt.Errorf("federation: authenticated peer requires an HTTP loopback SSH-forward endpoint without URL credentials, query or fragment")
	}
	return nil
}

func peerCredentialClient(client *http.Client, authority string, base *url.URL, ref string) (*http.Client, error) {
	if err := validatePeerCredential(base, ref); err != nil {
		return nil, err
	}
	if ref == "" {
		return client, nil
	}
	path, _ := credentialPath(ref) // validated above; never opened here
	transport := client.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	httpTransport, ok := transport.(*http.Transport)
	if !ok {
		return nil, fmt.Errorf("federation: authenticated peer requires an HTTP transport with a verifiable loopback connection")
	}
	guarded := httpTransport.Clone()
	// A bearer must never be delivered to an environment-selected proxy.
	guarded.Proxy = nil
	dial := guarded.DialContext
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	port := base.Port()
	if port == "" {
		port = "80"
	}
	expectedPort, _ := strconv.Atoi(port) // validated above
	expectedIP := net.ParseIP(base.Hostname())
	guarded.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, requestedPort, err := net.SplitHostPort(addr)
		if err != nil || host != base.Hostname() || requestedPort != port {
			return nil, &PeerAuthError{Authority: authority, Reason: ErrPeerOrigin}
		}
		conn, err := dial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		remote, ok := conn.RemoteAddr().(*net.TCPAddr)
		if !ok || !remote.IP.IsLoopback() || remote.Port != expectedPort || expectedIP != nil && !remote.IP.Equal(expectedIP) {
			_ = conn.Close()
			return nil, &PeerAuthError{Authority: authority, Reason: ErrPeerOrigin}
		}
		return conn, nil
	}
	cloned := *client
	cloned.Transport = &peerCredentialTransport{base: guarded, authority: authority, scheme: base.Scheme, host: base.Host, path: path}
	return &cloned, nil
}

type peerCredentialTransport struct {
	base                          http.RoundTripper
	authority, scheme, host, path string
}

func (t *peerCredentialTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Every redirect traverses this guard, even when a supplied CheckRedirect
	// accepts it. Refuse before sending either an envelope or a credential.
	if req.URL.Scheme != t.scheme || req.URL.Host != t.host {
		return nil, &PeerAuthError{Authority: t.authority, Reason: ErrPeerOrigin}
	}
	token, err := identity.ReadTokenFile(t.path)
	if err != nil {
		return nil, &PeerAuthError{Authority: t.authority, Reason: ErrPeerCredential}
	}
	cloned := req.Clone(req.Context())
	cloned.Header.Set("Authorization", "Bearer "+token)
	cloned.Header.Set(environment.ProtocolHeader, strconv.Itoa(environment.Protocol))
	resp, err := t.base.RoundTrip(cloned)
	if err != nil {
		if errors.Is(err, ErrPeerOrigin) {
			return nil, &PeerAuthError{Authority: t.authority, Reason: ErrPeerOrigin}
		}
		if req.Context().Err() != nil {
			return nil, req.Context().Err()
		}
		// A custom transport can include headers in an error. Retain no raw
		// error text from a credential-bearing request.
		return nil, fmt.Errorf("federation: peer %q transport failed", t.authority)
	}
	return resp, nil
}

func (t *peerCredentialTransport) CloseIdleConnections() {
	if closer, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}
