package client

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type MCPOptions struct {
	ClientOptions *mcpsdk.ClientOptions
	Profile       *string
	DiscoveryMode *string
}

// ConnectMCP uses this client's frozen credential and daemon HTTP/UDS dialer.
// It never opens local state, spawns upstreams, or retries a failed mutation.
func (c *Client) ConnectMCP(ctx context.Context, opts MCPOptions) (*mcpsdk.ClientSession, error) {
	if c == nil || c.http == nil {
		return nil, fmt.Errorf("MCP daemon client required")
	}
	query := url.Values{}
	if opts.Profile != nil {
		query.Set("profile", *opts.Profile)
	}
	if opts.DiscoveryMode != nil {
		query.Set("discovery_mode", *opts.DiscoveryMode)
	}
	endpoint := c.baseURL + "/mcp"
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	httpClient := *c.http
	// SSE is a long-lived body. Dial/request contexts bound connection setup,
	// while the SDK owns stream cancellation and DELETE on session close.
	httpClient.Timeout = 0
	base := httpClient.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	httpClient.Transport = mcpErrorTransport{base: base}
	return mcpsdk.NewClient(&mcpsdk.Implementation{Name: "tether-daemon-client", Version: "1"}, opts.ClientOptions).Connect(ctx, &mcpsdk.StreamableClientTransport{Endpoint: endpoint, HTTPClient: &httpClient, MaxRetries: -1}, nil)
}

type mcpErrorTransport struct{ base http.RoundTripper }

func (t mcpErrorTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	response, err := t.base.RoundTrip(r)
	return response, wrapIfUnreachable(err)
}
