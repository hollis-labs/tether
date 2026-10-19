package client

import (
	"context"
	"fmt"
	"net/url"
	"strconv"

	"github.com/hollis-labs/tether/internal/api"
)

// WorkstreamListOptions selects one bounded live page. Cursor is bound to filters.
type WorkstreamListOptions struct {
	Status     string
	WorkflowID string
	Ref        string
	Limit      int
	Cursor     string
}

func (c *Client) ListWorkstreamsPage(ctx context.Context, opts WorkstreamListOptions) (api.WorkstreamListResponse, error) {
	q := url.Values{}
	if opts.Status != "" {
		q.Set("status", opts.Status)
	}
	if opts.WorkflowID != "" {
		q.Set("workflow_id", opts.WorkflowID)
	}
	if opts.Ref != "" {
		q.Set("ref", opts.Ref)
	}
	if opts.Limit != 0 {
		q.Set("limit", strconv.Itoa(opts.Limit))
	}
	if opts.Cursor != "" {
		q.Set("cursor", opts.Cursor)
	}
	path := "/workstreams"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var out api.WorkstreamListResponse
	if err := c.getJSON(ctx, path, &out); err != nil {
		return out, wrapIfUnreachable(err)
	}
	return out, nil
}

func (c *Client) allWorkstreams(ctx context.Context, opts WorkstreamListOptions) ([]api.WorkstreamDTO, error) {
	out := make([]api.WorkstreamDTO, 0)
	seen := map[string]bool{}
	for {
		page, err := c.ListWorkstreamsPage(ctx, opts)
		if err != nil {
			return nil, err
		}
		out = append(out, page.Workstreams...)
		if !page.HasMore {
			if !page.Complete {
				return nil, fmt.Errorf("workstream server did not report listing completeness")
			}
			return out, nil
		}
		if page.NextCursor == "" || seen[page.NextCursor] {
			return nil, fmt.Errorf("workstream pagination returned a missing or repeated cursor")
		}
		seen[page.NextCursor] = true
		opts.Cursor = page.NextCursor
	}
}
