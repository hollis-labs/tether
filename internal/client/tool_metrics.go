package client

import (
	"context"
	"github.com/hollis-labs/tether/internal/telemetry"
	"net/url"
)

func (c *Client) ToolMetrics(ctx context.Context, req telemetry.MetricsRequest) (telemetry.MetricsResponse, error) {
	q := url.Values{}
	for _, f := range []struct{ key, value string }{{"tool", req.Tool}, {"upstream", req.Upstream}, {"since", req.Since}, {"until", req.Until}} {
		if f.value != "" {
			q.Set(f.key, f.value)
		}
	}
	path := "/events/tool-metrics"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var out telemetry.MetricsResponse
	if err := c.getJSON(ctx, path, &out); err != nil {
		return telemetry.MetricsResponse{}, wrapIfUnreachable(err)
	}
	return out, nil
}
