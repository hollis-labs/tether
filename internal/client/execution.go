package client

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"

	"github.com/hollis-labs/tether/internal/execpolicy"
)

// ExecutionStatus reads GET /execution/policy.
func (c *Client) ExecutionStatus(ctx context.Context) (execpolicy.Status, error) {
	var out execpolicy.Status
	if err := c.getJSON(ctx, "/execution/policy", &out); err != nil {
		return execpolicy.Status{}, wrapIfUnreachable(err)
	}
	return out, nil
}

// SetExecutionPolicy replaces the policy via PUT /execution/policy.
func (c *Client) SetExecutionPolicy(ctx context.Context, p execpolicy.Policy) (execpolicy.Status, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return execpolicy.Status{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.baseURL+"/execution/policy", bytes.NewReader(b))
	if err != nil {
		return execpolicy.Status{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return execpolicy.Status{}, wrapIfUnreachable(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return execpolicy.Status{}, readError(resp)
	}
	var out execpolicy.Status
	return out, json.NewDecoder(resp.Body).Decode(&out)
}
