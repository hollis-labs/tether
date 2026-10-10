package client

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/hollis-labs/tether/internal/api"
)

// SessionLogOptions selects a bounded snapshot or a byte range. Nil Offset
// selects the last Limit bytes; Generation guards a subsequent range.
type SessionLogOptions struct {
	Offset     *int64
	Limit      int
	Generation string
}

func (c *Client) SessionLog(ctx context.Context, id string, opts SessionLogOptions) (api.SessionLogResponse, error) {
	var out api.SessionLogResponse
	if id == "" || id == "." || id == ".." || strings.ContainsAny(id, "/\\") {
		return out, fmt.Errorf("invalid session id")
	}
	q := url.Values{}
	if opts.Offset != nil {
		if *opts.Offset < 0 {
			return out, fmt.Errorf("offset must be non-negative")
		}
		q.Set("offset", strconv.FormatInt(*opts.Offset, 10))
	}
	if opts.Limit < 0 || opts.Limit > api.MaxSessionLogBytes {
		return out, fmt.Errorf("invalid session log limit")
	}
	if opts.Limit != 0 {
		q.Set("limit", strconv.Itoa(opts.Limit))
	}
	if opts.Generation != "" {
		if opts.Offset == nil {
			return out, fmt.Errorf("generation requires an offset")
		}
		q.Set("generation", opts.Generation)
	}
	err := c.getJSON(ctx, "/sessions/"+url.PathEscape(id)+"/log?"+q.Encode(), &out)
	return out, err
}
