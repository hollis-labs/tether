package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCPInitializeTimeout allows cold daemon-owned upstreams to initialize.
const MCPInitializeTimeout = 15 * time.Second

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
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		var credential *credentialError
		if !errors.As(err, &credential) {
			err = fmt.Errorf("%w: %w", ErrDaemonUnreachable, err)
		}
	}
	return response, err
}

// ProbeMCP checks mounted, verified admission without initializing a view or
// starting upstreams. An admitted GET without an SDK session is rejected with
// mcp_session_required; that deliberate response proves the endpoint is ready.
func (c *Client) ProbeMCP(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/mcp", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("MCP endpoint unreachable")
	}
	defer resp.Body.Close()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if resp.StatusCode == http.StatusBadRequest && json.NewDecoder(io.LimitReader(resp.Body, 1024)).Decode(&body) == nil && body.Error.Code == "mcp_session_required" {
		return nil
	}
	return fmt.Errorf("MCP endpoint unavailable or credential not admitted (HTTP %d)", resp.StatusCode)
}
