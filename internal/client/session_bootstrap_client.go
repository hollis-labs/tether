package client

// session_bootstrap_client.go — typed Go client for the daemon's
// POST /sessions/bootstrap route (T08, messaging vNext). Mirrors
// internal/api/session_bootstrap.go.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// SessionBootstrapProviderMapping is one provider-native session id to
// record against the canonical session.
type SessionBootstrapProviderMapping struct {
	Owner           string `json:"owner"`
	Provider        string `json:"provider"`
	NativeSessionID string `json:"native_session_id"`
}

// SessionBootstrapRequest is the payload for Client.BootstrapSession.
// SessionID is required; every other field is optional.
type SessionBootstrapRequest struct {
	SessionID        string                            `json:"session_id"`
	Intent           string                            `json:"intent,omitempty"`
	ParentSessionID  string                            `json:"parent_session_id,omitempty"`
	LogicalAgentID   string                            `json:"logical_agent_id,omitempty"`
	Publication      string                            `json:"publication,omitempty"`
	ProviderMappings []SessionBootstrapProviderMapping `json:"provider_mappings,omitempty"`
}

// SessionBootstrapResult is the daemon's response.
type SessionBootstrapResult struct {
	SessionID string `json:"session_id"`
	Created   bool   `json:"created"`
}

// BootstrapSession POSTs /sessions/bootstrap. Idempotent: calling it
// again with the same SessionID never errors and never invents a
// competing identity (Created reports false on the repeat).
func (c *Client) BootstrapSession(ctx context.Context, req SessionBootstrapRequest) (SessionBootstrapResult, error) {
	if req.SessionID == "" {
		return SessionBootstrapResult{}, fmt.Errorf("client: bootstrap session: session_id is required")
	}
	body, err := json.Marshal(req)
	if err != nil {
		return SessionBootstrapResult{}, fmt.Errorf("marshal bootstrap request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/sessions/bootstrap", bytes.NewReader(body))
	if err != nil {
		return SessionBootstrapResult{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return SessionBootstrapResult{}, wrapIfUnreachable(err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return SessionBootstrapResult{}, readRegistryError(resp)
	}
	var out SessionBootstrapResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return SessionBootstrapResult{}, fmt.Errorf("decode bootstrap response: %w", err)
	}
	return out, nil
}
