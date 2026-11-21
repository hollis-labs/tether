package client

import (
	"context"
	"net/url"
	"strconv"

	"github.com/hollis-labs/tether/internal/agentlive"
)

// AgentsLiveOptions are the GET /agents/live filters; zero values are defaults.
type AgentsLiveOptions struct {
	Host              string
	Kind              string
	IncludeStale      bool
	StaleAfterSeconds int
	Limit             int
	Cursor            string
}

// AgentsLive reads GET /agents/live.
func (c *Client) AgentsLive(ctx context.Context, o AgentsLiveOptions) (agentlive.Response, error) {
	v := url.Values{}
	if o.Host != "" {
		v.Set("host", o.Host)
	}
	if o.Kind != "" {
		v.Set("kind", o.Kind)
	}
	if o.IncludeStale {
		v.Set("include_stale", "true")
	}
	if o.StaleAfterSeconds != 0 {
		v.Set("stale_after_seconds", strconv.Itoa(o.StaleAfterSeconds))
	}
	if o.Limit != 0 {
		v.Set("limit", strconv.Itoa(o.Limit))
	}
	if o.Cursor != "" {
		v.Set("cursor", o.Cursor)
	}
	path := "/agents/live"
	if len(v) > 0 {
		path += "?" + v.Encode()
	}
	var out agentlive.Response
	if err := c.getJSON(ctx, path, &out); err != nil {
		return agentlive.Response{}, wrapIfUnreachable(err)
	}
	return out, nil
}
