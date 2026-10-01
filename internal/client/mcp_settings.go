package client

import (
	"context"
	"github.com/hollis-labs/tether/internal/settings"
)

func (c *Client) GetMCPSettings(ctx context.Context) (settings.MCPSettings, error) {
	var out settings.MCPSettings
	err := c.getJSON(ctx, "/settings/mcp", &out)
	return out, err
}
