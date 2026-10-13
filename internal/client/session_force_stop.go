package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"

	"github.com/hollis-labs/tether/internal/api"
)

// ForceStopSession closes a session row that has no live worker or agent via
// POST /sessions/{id}/stop?force=true. A live session is refused with 409
// session_live unless allowLive is set.
func (c *Client) ForceStopSession(ctx context.Context, id, reason string, allowLive bool) (api.ForceStopResult, error) {
	q := url.Values{"force": {"true"}}
	if reason != "" {
		q.Set("reason", reason)
	}
	if allowLive {
		q.Set("allow_live", "true")
	}
	var out api.ForceStopResult
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/sessions/"+url.PathEscape(id)+"/stop?"+q.Encode(), nil)
	if err != nil {
		return out, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return out, wrapIfUnreachable(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return out, readError(resp)
	}
	return out, json.NewDecoder(resp.Body).Decode(&out)
}
