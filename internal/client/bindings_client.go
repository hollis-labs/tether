package client

// bindings_client.go — typed Go client for the daemon's /registry/bindings
// routes (T07's HTTP surface, given CLI/MCP/client parity in T08). Mirrors
// internal/api/bindings.go.
//
// Error mapping: HTTP 404 → wrapped registry.ErrNotFound; HTTP 409 →
// wrapped registry.ErrStaleGeneration or registry.ErrVisibilityConflict
// (the daemon's error message names which); other 4xx/5xx → a generic
// wrapped error surfacing the response body. Connection-level failures
// wrap ErrDaemonUnreachable, matching the rest of this package.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/hollis-labs/tether/internal/registry"
)

// BindingsClient is the typed handle for the daemon's /registry/bindings
// tree. Get one via Client.Bindings().
type BindingsClient struct {
	c *Client
}

// Bindings returns a BindingsClient bound to this Client.
func (c *Client) Bindings() *BindingsClient {
	return &BindingsClient{c: c}
}

// Lease POSTs /registry/bindings. capabilities=["pull-only"] is the only
// combination the daemon currently accepts from this external surface
// (published-local bridge leases); ttl<=0 requests no expiry.
func (bc *BindingsClient) Lease(ctx context.Context, targetURN, sessionID, hostID, attemptID string, capabilities []string, ttlSeconds int) (registry.RuntimeBinding, error) {
	body, err := json.Marshal(map[string]any{
		"target_urn":   targetURN,
		"session_id":   sessionID,
		"host_id":      hostID,
		"attempt_id":   attemptID,
		"capabilities": capabilities,
		"ttl_seconds":  ttlSeconds,
	})
	if err != nil {
		return registry.RuntimeBinding{}, fmt.Errorf("marshal lease request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, bc.c.baseURL+"/registry/bindings", bytes.NewReader(body))
	if err != nil {
		return registry.RuntimeBinding{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := bc.c.http.Do(req)
	if err != nil {
		return registry.RuntimeBinding{}, wrapIfUnreachable(err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusCreated {
		return registry.RuntimeBinding{}, readBindingError(resp)
	}
	var out registry.RuntimeBinding
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return registry.RuntimeBinding{}, fmt.Errorf("decode lease response: %w", err)
	}
	return out, nil
}

// Renew POSTs /registry/bindings/{id}/renew.
func (bc *BindingsClient) Renew(ctx context.Context, bindingID string, ttlSeconds int) (registry.RuntimeBinding, error) {
	body, err := json.Marshal(map[string]any{"ttl_seconds": ttlSeconds})
	if err != nil {
		return registry.RuntimeBinding{}, fmt.Errorf("marshal renew request: %w", err)
	}
	path := "/registry/bindings/" + url.PathEscape(bindingID) + "/renew"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, bc.c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return registry.RuntimeBinding{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := bc.c.http.Do(req)
	if err != nil {
		return registry.RuntimeBinding{}, wrapIfUnreachable(err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return registry.RuntimeBinding{}, readBindingError(resp)
	}
	var out registry.RuntimeBinding
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return registry.RuntimeBinding{}, fmt.Errorf("decode renew response: %w", err)
	}
	return out, nil
}

// Revoke POSTs /registry/bindings/{id}/revoke. Idempotent at the server.
func (bc *BindingsClient) Revoke(ctx context.Context, bindingID string) error {
	path := "/registry/bindings/" + url.PathEscape(bindingID) + "/revoke"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, bc.c.baseURL+path, nil)
	if err != nil {
		return err
	}
	resp, err := bc.c.http.Do(req)
	if err != nil {
		return wrapIfUnreachable(err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusNoContent {
		return readBindingError(resp)
	}
	return nil
}

// Current GETs /registry/bindings?target_urn=&current=true.
func (bc *BindingsClient) Current(ctx context.Context, targetURN string) (registry.RuntimeBinding, error) {
	q := url.Values{"target_urn": {targetURN}, "current": {"true"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, bc.c.baseURL+"/registry/bindings?"+q.Encode(), nil)
	if err != nil {
		return registry.RuntimeBinding{}, err
	}
	resp, err := bc.c.http.Do(req)
	if err != nil {
		return registry.RuntimeBinding{}, wrapIfUnreachable(err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return registry.RuntimeBinding{}, readBindingError(resp)
	}
	var out registry.RuntimeBinding
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return registry.RuntimeBinding{}, fmt.Errorf("decode current binding response: %w", err)
	}
	return out, nil
}

// ListForTarget GETs /registry/bindings?target_urn= (every binding ever
// leased for targetURN, newest generation first — an audit/debugging view).
func (bc *BindingsClient) ListForTarget(ctx context.Context, targetURN string) ([]registry.RuntimeBinding, error) {
	q := url.Values{"target_urn": {targetURN}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, bc.c.baseURL+"/registry/bindings?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := bc.c.http.Do(req)
	if err != nil {
		return nil, wrapIfUnreachable(err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return nil, readBindingError(resp)
	}
	var env struct {
		Bindings []registry.RuntimeBinding `json:"bindings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return nil, fmt.Errorf("decode list bindings response: %w", err)
	}
	if env.Bindings == nil {
		env.Bindings = []registry.RuntimeBinding{}
	}
	return env.Bindings, nil
}

// readBindingError reads a daemon error envelope and maps it to a typed
// registry error where possible, mirroring readRegistryError's shape.
func readBindingError(resp *http.Response) error {
	body, _ := io.ReadAll(resp.Body)
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &env)

	switch resp.StatusCode {
	case http.StatusNotFound:
		// Wraps BOTH sentinels: the daemon's actual source is
		// registry.ErrBindingNotFound (bindings.go), but this client
		// also wraps the more general registry.ErrNotFound so a caller
		// checking either one (matching the convention every other
		// typed client in this package uses for 404) gets a correct
		// match, not a latent trap.
		return fmt.Errorf("daemon %d (%s): %s: %w: %w", resp.StatusCode, env.Error.Code, env.Error.Message, registry.ErrBindingNotFound, registry.ErrNotFound)
	case http.StatusConflict:
		if strings.Contains(env.Error.Message, registry.ErrVisibilityConflict.Error()) {
			return fmt.Errorf("daemon %d (%s): %s: %w", resp.StatusCode, env.Error.Code, env.Error.Message, registry.ErrVisibilityConflict)
		}
		return fmt.Errorf("daemon %d (%s): %s: %w", resp.StatusCode, env.Error.Code, env.Error.Message, registry.ErrStaleGeneration)
	}
	if env.Error.Message != "" {
		return fmt.Errorf("daemon %d (%s): %s", resp.StatusCode, env.Error.Code, env.Error.Message)
	}
	return fmt.Errorf("daemon %d: %s", resp.StatusCode, string(body))
}
