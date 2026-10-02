package mcpadapter

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/hollis-labs/tether/internal/callcontext"
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
	origin, err := url.Parse(endpoint)
	if err != nil || origin.Host == "" || origin.User != nil || (origin.Scheme != "http" && origin.Scheme != "https") {
		return nil, fmt.Errorf("invalid upstream service credential endpoint")
	}
	token, err := identity.ReadBearerTokenFile(tokenFile)
	if err != nil {
		return nil, fmt.Errorf("read upstream service credential: %w", err)
	}
	return func(headers map[string]string, seconds int) *http.Client {
		fixed := make(map[string]string, len(headers))
		for name, value := range headers {
			fixed[name] = value
		}
		client := &http.Client{Transport: &forwardedContextTransport{base: http.DefaultTransport, origin: origin, serviceToken: token, headers: fixed}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		if seconds > 0 {
			client.Timeout = time.Duration(seconds) * time.Second
		}
		return client
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
		p.SessionID != "" && s.SessionID == p.SessionID {
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

func setContextHeader(headers http.Header, name, value string) {
	if value != "" {
		headers.Set(name, value)
	}
}
