package client

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"

	directory "github.com/hollis-labs/tether/internal/environmentdirectory"
)

func (c *Client) ListEnvironments(ctx context.Context) ([]directory.Record, error) {
	var out struct {
		Environments []directory.Record `json:"environments"`
	}
	err := c.getJSON(ctx, "/environments", &out)
	return out.Environments, err
}
func (c *Client) GetEnvironment(ctx context.Context, id string) (directory.Record, error) {
	var out directory.Record
	err := c.getJSON(ctx, "/environments/"+url.PathEscape(id), &out)
	return out, err
}
func (c *Client) RegisterEnvironment(ctx context.Context, in directory.Registration) (directory.Record, error) {
	return c.environmentMutation(ctx, http.MethodPost, "/environments", in)
}
func (c *Client) RenameEnvironment(ctx context.Context, id, label string) (directory.Record, error) {
	return c.environmentMutation(ctx, http.MethodPatch, "/environments/"+url.PathEscape(id), map[string]string{"label": label})
}
func (c *Client) RetireEnvironment(ctx context.Context, id string) (directory.Record, error) {
	return c.environmentMutation(ctx, http.MethodDelete, "/environments/"+url.PathEscape(id), nil)
}
func (c *Client) environmentMutation(ctx context.Context, method, path string, in any) (directory.Record, error) {
	var out directory.Record
	body, err := json.Marshal(in)
	if err != nil {
		return out, err
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return out, err
	}
	// Do not permit the transport to replay even an ostensibly keyed mutation.
	req.GetBody = nil
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return out, wrapIfUnreachable(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return out, readError(resp)
	}
	err = json.NewDecoder(resp.Body).Decode(&out)
	return out, err
}
