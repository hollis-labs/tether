package client

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"

	"github.com/hollis-labs/tether/internal/config"
)

// ReloadCatalog POSTs /catalog/reload: the daemon re-reads its catalog, and
// swaps it in only if it validates.
func (c *Client) ReloadCatalog(ctx context.Context) (*config.ReloadReport, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/catalog/reload", bytes.NewReader(nil))
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, wrapIfUnreachable(err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return nil, readError(resp)
	}
	var out config.ReloadReport
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}
