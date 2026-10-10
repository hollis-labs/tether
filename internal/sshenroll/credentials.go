package sshenroll

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	tetherclient "github.com/hollis-labs/substrate/mesh/tetherclient"
	"github.com/hollis-labs/tether/internal/environment"
	"github.com/hollis-labs/tether/internal/identity"
)

func enrollmentHTTPClient(input *http.Client, baseURL string, remotePort int) (*http.Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" || u.Hostname() != "127.0.0.1" || u.Port() == "" {
		return nil, problem("forward", "invalid-origin", "Enrollment requires an explicit SSH-forwarded IPv4 loopback endpoint.")
	}
	c := http.Client{}
	if input != nil {
		c = *input
	}
	base := c.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	transport, ok := base.(*http.Transport)
	if !ok {
		return nil, problem("forward", "unverified-transport", "Use a supported standard HTTP transport.")
	}
	t := transport.Clone()
	t.Proxy = nil
	t.DisableKeepAlives = true
	t.ForceAttemptHTTP2 = false
	dial := t.DialContext
	if dial == nil {
		dial = (&net.Dialer{Timeout: 5 * time.Second}).DialContext
	}
	t.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != u.Host {
			return nil, errors.New("enrollment origin refused")
		}
		conn, err := dial(ctx, network, address)
		if err != nil {
			return nil, errors.New("enrollment forward unavailable")
		}
		remote, ok := conn.RemoteAddr().(*net.TCPAddr)
		if !ok || remote.IP.String() != "127.0.0.1" || portText(remote.Port) != u.Port() {
			_ = conn.Close()
			return nil, errors.New("enrollment connection origin refused")
		}
		return conn, nil
	}
	c.Transport = &forwardTransport{base: t, origin: u.Host, host: "127.0.0.1:" + portText(remotePort)}
	c.Jar = nil
	c.Timeout = 0
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &c, nil
}

func requestJSON(ctx context.Context, c *http.Client, baseURL, method, path string, input, out any, token string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var body io.Reader
	if input != nil {
		b, _ := json.Marshal(input)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, baseURL+path, body)
	if err != nil {
		return problem("http", "invalid-request", "Invalid enrollment request.")
	}
	req.Header.Set(environment.ProtocolHeader, portText(environment.Protocol))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return problem("http", "transport-unavailable", "The owned loopback forward is unavailable; private transport output was withheld.")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		code := "request-refused"
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			code = "credential-refused"
		}
		return problem("http", code, "The worker refused the request; no credential or peer response text is reported.")
	}
	if out == nil {
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (64<<10)+1))
	if err != nil || len(data) > 64<<10 || json.Unmarshal(data, out) != nil {
		return problem("http", "invalid-response", "The worker response was malformed or oversized; private response text was withheld.")
	}
	return nil
}
func waitDescriptor(ctx context.Context, c *http.Client, base string, r Receipt) (environment.Descriptor, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	for {
		var d environment.Descriptor
		err := requestJSON(ctx, c, base, http.MethodGet, environment.DescriptorPath, nil, &d, "")
		if err == nil {
			if d.EnvironmentID != r.EnvironmentID {
				return d, problem("descriptor", "identity-mismatch", "SSH and HTTP worker identities disagree; no credential was sent.")
			}
			if d.Protocol != environment.Protocol {
				return d, problem("descriptor", "protocol-mismatch", "Update the hub or worker to the same protocol before pairing.")
			}
			if d.ServerVersion != r.Version || d.Platform.OS != "linux" || d.Platform.Arch != r.Preflight.Arch {
				return d, problem("descriptor", "release-mismatch", "The worker descriptor differs from the verified release or preflight platform.")
			}
			return d, nil
		}
		select {
		case <-ctx.Done():
			return d, problem("descriptor", "readiness-timeout", "The loopback descriptor did not become ready; retain the worker receipt.")
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func validGrant(g identity.IssuedGrant, scopes []string) bool {
	return strings.HasPrefix(g.ID, "grant_") && validOpaque("tth_"+strings.TrimPrefix(g.ID, "grant_"), "tth_") && strings.HasPrefix(g.Code, "tpg_") && validOpaque(g.Code, "tpg_") && g.ExpiresAt.After(time.Now()) && g.ExpiresAt.Before(time.Now().Add(identity.MaxGrantTTL+time.Second)) && reflect.DeepEqual(g.Scopes, normalizedScopes(scopes))
}
func normalizedScopes(scopes []string) []string {
	v, _ := identity.NormalizeDeviceScopes(scopes)
	return v
}
func exchange(ctx context.Context, c *http.Client, base, code string, scopes []string) (identity.DeviceExchange, error) {
	var d identity.DeviceExchange
	body := struct {
		Code   string   `json:"code"`
		Scopes []string `json:"scopes"`
	}{code, scopes}
	if err := requestJSON(ctx, c, base, http.MethodPost, "/auth/pair/exchange", body, &d, ""); err != nil {
		return d, err
	}
	if (!strings.HasPrefix(d.Principal.ID, "msg://device/") || !validOpaque("tth_"+strings.TrimPrefix(d.Principal.ID, "msg://device/"), "tth_")) || d.Principal.Kind != "device" || !reflect.DeepEqual(d.Principal.Scopes, normalizedScopes(scopes)) || d.Principal.ExpiresAt == nil || !d.Principal.ExpiresAt.After(time.Now()) || !validOpaque(d.Token, "tth_") {
		return identity.DeviceExchange{}, problem("pair", "invalid-device", "The worker returned an invalid device credential; retain the uncertain outcome.")
	}
	return d, nil
}
func (s *receipts) saveCredential(token string) (string, error) {
	if !validOpaque(token, "tth_") {
		return "", problem("credential", "invalid-token", "The device token is invalid.")
	}
	if err := privateAtomic(s.root, "device.token", []byte(token+"\n"), true); err != nil {
		return "", err
	}
	path := filepath.Join(s.root, "device.token")
	if _, err := identity.ReadTokenFile(path); err != nil {
		return "", problem("credential", "invalid-private-file", "The persisted device token could not be verified.")
	}
	return (&url.URL{Scheme: "file", Path: path}).String(), nil
}
func verifyCredential(ctx context.Context, c *http.Client, base string, r Receipt) error {
	client, err := tetherclient.NewEnvironmentClient(tetherclient.EnvironmentTarget{
		EnvironmentID: r.EnvironmentID, Authority: r.Authority,
		Routes:              []tetherclient.EnvironmentRoute{{BaseURL: base}},
		CredentialReference: r.CredentialReference,
	}, tetherclient.EnvironmentOptions{
		HTTPClient: c, ProbeTimeout: 2500 * time.Millisecond, ConnectTimeout: 5 * time.Second,
		ResolveCredential: func(ctx context.Context, reference string) (string, error) {
			if err := ctx.Err(); err != nil {
				return "", err
			}
			u, err := url.Parse(reference)
			if err != nil || u.Scheme != "file" || u.Host != "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || !filepath.IsAbs(u.Path) {
				return "", errors.New("invalid explicit credential reference")
			}
			return identity.ReadTokenFile(u.Path)
		},
	})
	if err != nil {
		return problem("credential", "invalid-reference", "An explicit pinned environment and private device reference are required.")
	}
	if _, err = client.Connect(ctx); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return problem("credential", "credential-refused", "Verify the retained worker identity and private device file; no operator or ambient fallback is used.")
	}
	return nil
}

func validOpaque(token, prefix string) bool {
	if len(token) != 47 || !strings.HasPrefix(token, prefix) {
		return false
	}
	b, e := base64.RawURLEncoding.DecodeString(token[4:])
	return e == nil && len(b) == 32 && base64.RawURLEncoding.EncodeToString(b) == token[4:]
}

type forwardTransport struct {
	base         *http.Transport
	origin, host string
}

func (t *forwardTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme != "http" || r.URL.Host != t.origin {
		return nil, errors.New("enrollment origin refused")
	}
	q := r.Clone(r.Context())
	q.Host = t.host
	return t.base.RoundTrip(q)
}
func (t *forwardTransport) CloseIdleConnections() { t.base.CloseIdleConnections() }
