package client

import (
	"context"
	"net/url"
	"strconv"

	"github.com/hollis-labs/tether/internal/app/routingcap"
	"github.com/hollis-labs/tether/internal/messaging/channels"
)

// List implements the channel consumer port through the daemon.
func (c *Client) List(ctx context.Context, as string) ([]channels.Channel, error) {
	var result struct {
		Channels []channels.Channel `json:"channels"`
	}
	err := c.getJSON(ctx, "/channels?"+url.Values{"as": {as}}.Encode(), &result)
	return result.Channels, err
}

func (c *Client) History(ctx context.Context, name, as string, since int64, limit int) (channels.Page, error) {
	q := url.Values{"as": {as}, "since": {strconv.FormatInt(since, 10)}}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	return c.channelPage(ctx, name, q)
}

func (c *Client) Latest(ctx context.Context, name, as string, last int) (channels.Page, error) {
	return c.channelPage(ctx, name, url.Values{"as": {as}, "last": {strconv.Itoa(last)}})
}

func (c *Client) channelPage(ctx context.Context, name string, q url.Values) (channels.Page, error) {
	var page channels.Page
	if err := channels.ValidateName(name); err != nil {
		return page, err
	}
	err := c.getJSON(ctx, "/channels/"+url.PathEscape(name)+"/messages?"+q.Encode(), &page)
	return page, err
}

func (c *Client) RoutingCapabilities(ctx context.Context, sessionID string) (routingcap.RoutingCapabilitiesResponse, error) {
	var result routingcap.RoutingCapabilitiesResponse
	q := url.Values{}
	if sessionID != "" {
		q.Set("session_id", sessionID)
	}
	err := c.getJSON(ctx, "/routing/capabilities?"+q.Encode(), &result)
	return result, err
}
