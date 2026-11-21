package client

import (
	"context"
	"github.com/hollis-labs/tether/internal/api"
	"net/http"
)

// RegisterCorrelation acknowledges queue admission, never remote persistence.
func (c *Client) RegisterCorrelation(ctx context.Context, in api.CorrelationRegisterRequest) (api.CorrelationRegisterResponse, error) {
	var out api.CorrelationRegisterResponse
	err := c.postWorkstreamJSON(ctx, "/correlation/register", in, &out, http.StatusAccepted)
	return out, err
}
