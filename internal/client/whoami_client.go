package client

// whoami_client.go — typed Go client for the daemon's GET /whoami
// self-discovery route (T08, messaging vNext). Mirrors internal/api/whoami.go.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/registry"
)

// WhoamiResult mirrors internal/api's whoamiResponse. Kept as its own
// type here (rather than importing internal/api) to preserve this
// package's existing no-cycle boundary with internal/api.
type WhoamiResult struct {
	MCPGrant    *config.SessionMCPGrantImpact `json:"mcp_grant,omitempty"`
	URN         string                        `json:"urn"`
	Profile     *registry.Profile             `json:"profile,omitempty"`
	ExternalIDs []registry.ExternalID         `json:"external_ids,omitempty"`
	Groups      []registry.Profile            `json:"groups,omitempty"`
	Binding     *registry.RuntimeBinding      `json:"binding,omitempty"`
	// CanonicalURN and Aliases are set when the address has alias rows
	// (CW-20261114-0067): where its mail is stored, and its other names.
	CanonicalURN string   `json:"canonical_urn,omitempty"`
	Aliases      []string `json:"aliases,omitempty"`
}

// Whoami GETs /whoami?as=<urn>. Same-host, self-asserted trust model
// (ADR 0045) -- as is whatever identity the caller claims to be; every
// sub-lookup in the result is independently best-effort (nil/empty when
// that lookup found nothing, never because of a swallowed error -- see
// internal/api/whoami.go).
func (c *Client) Whoami(ctx context.Context, as string) (WhoamiResult, error) {
	q := url.Values{"as": {as}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/whoami?"+q.Encode(), nil)
	if err != nil {
		return WhoamiResult{}, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return WhoamiResult{}, wrapIfUnreachable(err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return WhoamiResult{}, readRegistryError(resp)
	}
	var out WhoamiResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return WhoamiResult{}, fmt.Errorf("decode whoami response: %w", err)
	}
	return out, nil
}
