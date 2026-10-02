package mcpadapter

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/hollis-labs/tether/internal/callcontext"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/identity"
)

type forwardedToolCallKey struct{}

func withForwardedToolCall(ctx context.Context) context.Context {
	return context.WithValue(ctx, forwardedToolCallKey{}, true)
}

// forwardedContextTransport belongs to a single daemon-owned catalog entry.
// Its service credential is upstream-issued and never a caller's bearer.
// Attribution lives only on a cloned request, never on the shared connection.
type forwardedContextTransport struct {
	base         http.RoundTripper
	origin       *url.URL
	serviceToken string
	headers      map[string]string
}

// serviceHTTPClientFactory is constructed with the catalog entry, before
// go-mcp's entry-blind builder seam. Reconnects reuse its entry-specific policy.
func serviceHTTPClientFactory(endpoint, tokenFile string) (func(map[string]string, int) *http.Client, error) {
	if !filepath.IsAbs(tokenFile) {
		return nil, fmt.Errorf("upstream service credential path must be absolute")
	}
	token, err := identity.ReadBearerTokenFile(tokenFile)
	if err != nil {
		return nil, fmt.Errorf("read upstream service credential: %w", err)
	}
	return serviceHTTPClientFactoryForToken(endpoint, token, false)
}

func serviceHTTPClientFactoryForToken(endpoint, token string, sse bool) (func(map[string]string, int) *http.Client, error) {
	origin, err := url.Parse(endpoint)
	if err != nil || origin.Host == "" || origin.User != nil || (origin.Scheme != "http" && origin.Scheme != "https") {
		return nil, fmt.Errorf("invalid upstream service credential endpoint")
	}
	return func(headers map[string]string, seconds int) *http.Client {
		fixed := make(map[string]string, len(headers))
		for name, value := range headers {
			fixed[name] = value
		}
		base := http.DefaultTransport
		if sse {
			base = &sseLifetimeTransport{base: base}
		}
		client := &http.Client{Transport: &forwardedContextTransport{base: base, origin: origin, serviceToken: token, headers: fixed}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		if seconds > 0 {
			client.Timeout = time.Duration(seconds) * time.Second
		}
		return client
	}, nil
}

// daemonHTTPPolicies loads credentials only into a private daemon-owner copy.
// The existing pool redaction paths then include the service secret alongside
// ordinary upstream credentials, without altering the authored catalog.
func daemonHTTPPolicies(entries []config.MCPServerEntry) ([]config.MCPServerEntry, func(config.MCPServerEntry) (func(map[string]string, int) *http.Client, error), error) {
	private := append([]config.MCPServerEntry(nil), entries...)
	builders := make(map[string]func(map[string]string, int) *http.Client)
	for i, entry := range private {
		if !entry.IsEnabled() || entry.ProxyServiceTokenFile == "" {
			continue
		}
		if (entry.Transport != "http" && entry.Transport != "sse") || entry.Token != "" {
			return nil, nil, fmt.Errorf("proxy service credential requires HTTP/SSE and cannot be combined with token")
		}
		if !filepath.IsAbs(entry.ProxyServiceTokenFile) {
			return nil, nil, fmt.Errorf("upstream service credential path must be absolute")
		}
		token, err := identity.ReadBearerTokenFile(entry.ProxyServiceTokenFile)
		if err != nil {
			return nil, nil, fmt.Errorf("read upstream service credential: %w", err)
		}
		build, err := serviceHTTPClientFactoryForToken(entry.URL, token, entry.Transport == "sse")
		if err != nil {
			return nil, nil, err
		}
		private[i].Token = token
		builders[entry.ID] = build
	}
	return private, func(entry config.MCPServerEntry) (func(map[string]string, int) *http.Client, error) {
		return builders[entry.ID], nil
	}, nil
}

func (t *forwardedContextTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != t.origin.Scheme || !strings.EqualFold(req.URL.Host, t.origin.Host) {
		return nil, fmt.Errorf("upstream credential transport refused a different origin")
	}
	out := req.Clone(req.Context())
	if out.Header == nil {
		out.Header = make(http.Header)
	}
	for name, value := range t.headers {
		out.Header.Set(name, value)
	}
	for name := range out.Header {
		lower := strings.ToLower(name)
		if strings.HasPrefix(lower, "x-tether-") || strings.HasPrefix(lower, "x-forwarded-") {
			delete(out.Header, name)
		}
	}
	out.Header.Set("Authorization", "Bearer "+t.serviceToken)
	// A trusted source string alone is not an authentication boundary. Require
	// the admitted session principal and the daemon-resolved snapshot to agree.
	p, admitted := identity.FromContext(req.Context())
	s, resolved := callcontext.FromContext(req.Context())
	toolCall, _ := req.Context().Value(forwardedToolCallKey{}).(bool)
	if toolCall && req.Method == http.MethodPost && admitted && resolved && s.Verified && s.Source == "daemon" &&
		p.Kind == "session" && s.PrincipalKind == p.Kind && s.PrincipalID == p.ID && p.ID != "" &&
		p.SessionID != "" && s.SessionID == p.SessionID && safeForwardedContext(s) {
		out.Header.Set("X-Forwarded-User-Id", "session:"+s.SessionID)
		out.Header.Set("X-Tether-Session-Id", s.SessionID)
		setContextHeader(out.Header, "X-Tether-Agent-Urn", s.AgentURN)
		setContextHeader(out.Header, "X-Tether-Workstream-Id", s.WorkstreamID)
		setContextHeader(out.Header, "X-Tether-Launch-Id", s.LaunchID)
		setContextHeader(out.Header, "X-Tether-Project-Id", s.ProjectID)
		setContextHeader(out.Header, "X-Tether-Logical-Agent-Id", s.LogicalAgentID)
	}
	return t.base.RoundTrip(out)
}

func safeForwardedContext(s callcontext.Snapshot) bool {
	for _, value := range []string{s.SessionID, s.AgentURN, s.WorkstreamID, s.LaunchID, s.ProjectID, s.LogicalAgentID} {
		if len(value) > 512 {
			return false
		}
		for _, c := range value {
			if c <= 0x20 || c >= 0x7f {
				return false
			}
		}
	}
	return true
}

func setContextHeader(headers http.Header, name, value string) {
	if value != "" {
		headers.Set(name, value)
	}
}
