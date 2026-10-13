package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/catalogapply"
)

// CatalogApplyClient calls the daemon's /catalog/plans and /catalog/backups
// routes (CW-20261012-0124).
type CatalogApplyClient struct{ c *Client }

// CatalogApply returns the catalog plan/approve/apply client.
func (c *Client) CatalogApply() *CatalogApplyClient { return &CatalogApplyClient{c: c} }

func (cc *CatalogApplyClient) do(ctx context.Context, method, path string, in, out any) error {
	var body *bytes.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
		body = bytes.NewReader(b)
	} else {
		body = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, cc.c.baseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := cc.c.http.Do(req)
	if err != nil {
		return wrapIfUnreachable(err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return readError(resp)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Plan reviews a bundle (or, with req.RollbackOf, a rollback) without changing the catalog.
func (cc *CatalogApplyClient) Plan(ctx context.Context, req api.CatalogPlanRequest) (*catalogapply.Plan, error) {
	var out catalogapply.Plan
	if err := cc.do(ctx, http.MethodPost, "/catalog/plans", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Get reads a plan.
func (cc *CatalogApplyClient) Get(ctx context.Context, id string) (*catalogapply.Plan, error) {
	var out catalogapply.Plan
	if err := cc.do(ctx, http.MethodGet, "/catalog/plans/"+url.PathEscape(id), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Approve records the operator's approval. Only the local operator, on the
// daemon's unix socket and holding the approval passphrase, succeeds.
func (cc *CatalogApplyClient) Approve(ctx context.Context, id string, req api.CatalogApproveRequest) (*catalogapply.Plan, error) {
	var out catalogapply.Plan
	if err := cc.do(ctx, http.MethodPost, "/catalog/plans/"+url.PathEscape(id)+"/approve", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Apply installs an approved plan.
func (cc *CatalogApplyClient) Apply(ctx context.Context, id string) (*catalogapply.Result, error) {
	var out catalogapply.Result
	if err := cc.do(ctx, http.MethodPost, "/catalog/plans/"+url.PathEscape(id)+"/apply", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Backups lists catalog backups, newest first.
func (cc *CatalogApplyClient) Backups(ctx context.Context) ([]catalogapply.BackupInfo, error) {
	var out struct {
		Backups []catalogapply.BackupInfo `json:"backups"`
	}
	if err := cc.do(ctx, http.MethodGet, "/catalog/backups", nil, &out); err != nil {
		return nil, err
	}
	return out.Backups, nil
}
