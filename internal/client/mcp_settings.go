package client

import (
	"context"
	"encoding/json"
	"github.com/hollis-labs/tether/internal/settings"
	"log/slog"
	"net/http"
)

func (c *Client) GetMCPSettings(ctx context.Context) (settings.MCPSettings, error) {
	var out settings.MCPSettings
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/settings/mcp", nil)
	if err != nil {
		return out, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return out, wrapIfUnreachable(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		slog.Info("daemon has no MCP settings endpoint; using no persisted discovery mode")
		return out, nil
	}
	if resp.StatusCode != http.StatusOK {
		return out, readError(resp)
	}
	err = json.NewDecoder(resp.Body).Decode(&out)
	return out, err
}
