package client

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/hollis-labs/tether/internal/api"
)

// The read side of a session and the proxy's call log, over the daemon API.
// The daemon-only `mux mcp` Tether plants in an agent has no state database
// of its own and reads these here (CW-20261001-0173).

// SessionEvents fetches a session's event history from
// GET /sessions/{id}/events, oldest first. cursor is the seq to continue
// after; 0 starts at the beginning. limit <= 0 is the daemon's default.
func (c *Client) SessionEvents(ctx context.Context, id string, limit int, cursor int64) (api.EventListResponse, error) {
	params := url.Values{}
	if limit > 0 {
		params.Set("limit", strconv.Itoa(limit))
	}
	if cursor > 0 {
		params.Set("cursor", strconv.FormatInt(cursor, 10))
	}
	path := "/sessions/" + url.PathEscape(id) + "/events"
	if len(params) > 0 {
		path += "?" + params.Encode()
	}
	var res api.EventListResponse
	if err := c.getJSON(ctx, path, &res); err != nil {
		return api.EventListResponse{}, err
	}
	return res, nil
}

// SessionCheckpoints fetches the checkpoints of a session's logical agent
// from GET /sessions/{id}/checkpoints.
func (c *Client) SessionCheckpoints(ctx context.Context, id string) ([]api.CheckpointDTO, error) {
	var res api.CheckpointListResponse
	if err := c.getJSON(ctx, "/sessions/"+url.PathEscape(id)+"/checkpoints", &res); err != nil {
		return nil, err
	}
	return res.Checkpoints, nil
}

// SessionAttachments fetches a session's client attach/detach records from
// GET /sessions/{id}/attachments.
func (c *Client) SessionAttachments(ctx context.Context, id string) ([]api.AttachmentDTO, error) {
	var res api.AttachmentListResponse
	if err := c.getJSON(ctx, "/sessions/"+url.PathEscape(id)+"/attachments", &res); err != nil {
		return nil, err
	}
	return res.Attachments, nil
}

// SessionHealth fetches a running session's live runtime health from
// GET /sessions/{id}/health. A session that is not running is a 409.
func (c *Client) SessionHealth(ctx context.Context, id string) (api.RuntimeHealthResponse, error) {
	var res api.RuntimeHealthResponse
	if err := c.getJSON(ctx, "/sessions/"+url.PathEscape(id)+"/health", &res); err != nil {
		return api.RuntimeHealthResponse{}, err
	}
	return res, nil
}

// ProxyEventsQuery filters GET /proxy/events. Zero values match everything.
type ProxyEventsQuery struct {
	SessionID  string
	Server     string
	ToolName   string // prefix
	Limit      int
	ErrorsOnly bool
	Since      time.Time // exclusive lower bound
}

// ProxyEvents fetches recorded proxy tool calls from GET /proxy/events,
// newest first, with every filter the endpoint takes.
func (c *Client) ProxyEvents(ctx context.Context, q ProxyEventsQuery) ([]api.ProxyEventDTO, error) {
	params := url.Values{}
	if q.SessionID != "" {
		params.Set("session_id", q.SessionID)
	}
	if q.Server != "" {
		params.Set("server", q.Server)
	}
	if q.ToolName != "" {
		params.Set("tool_name", q.ToolName)
	}
	if q.Limit > 0 {
		params.Set("limit", strconv.Itoa(q.Limit))
	}
	if q.ErrorsOnly {
		params.Set("errors_only", "true")
	}
	if !q.Since.IsZero() {
		params.Set("since", q.Since.UTC().Format(time.RFC3339Nano))
	}
	path := "/proxy/events"
	if len(params) > 0 {
		path += "?" + params.Encode()
	}
	var res api.ProxyEventListResponse
	if err := c.getJSON(ctx, path, &res); err != nil {
		return nil, err
	}
	return res.Events, nil
}

// IngestProxyEvent records a proxied tool call with POST /proxy/events. See
// api.ProxyEventIngestRequest for Phase and Publish.
func (c *Client) IngestProxyEvent(ctx context.Context, req api.ProxyEventIngestRequest) error {
	return c.postWorkstreamJSON(ctx, "/proxy/events", req, nil, http.StatusCreated)
}
