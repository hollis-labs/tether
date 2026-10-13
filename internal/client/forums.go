package client

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	f "github.com/hollis-labs/go-forums"
	"github.com/hollis-labs/tether/internal/api"
)

// ForumExecute always goes through the daemon, including reads and policy.
func (c *Client) ForumExecute(ctx context.Context, op string, in f.Request) (any, error) {
	for _, id := range []string{in.ForumID, in.TopicID, in.PostID} {
		if strings.ContainsAny(id, "/?#") {
			return nil, f.ErrInvalid
		}
	}
	method, path, e := api.ForumRoute(op, in)
	if e != nil {
		return nil, e
	}
	var body []byte
	if method == http.MethodGet {
		b, e := json.Marshal(in)
		if e != nil {
			return nil, e
		}
		var m map[string]any
		if e = json.Unmarshal(b, &m); e != nil {
			return nil, e
		}
		q := url.Values{}
		for k, v := range m {
			switch x := v.(type) {
			case string:
				q.Set(k, x)
			default:
				b, e := json.Marshal(x)
				if e != nil {
					return nil, e
				}
				q.Set(k, string(b))
			}
		}
		path += "?" + q.Encode()
	} else {
		body, e = json.Marshal(in)
		if e != nil {
			return nil, e
		}
	}
	req, e := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(body))
	if e != nil {
		return nil, e
	}
	if method != http.MethodGet {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, e := c.http.Do(req)
	if e != nil {
		return nil, wrapIfUnreachable(e)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		return nil, readError(resp)
	}
	var out any
	e = json.NewDecoder(resp.Body).Decode(&out)
	return out, e
}
