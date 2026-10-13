package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/catalogapply"
	"github.com/hollis-labs/tether/internal/catalogcheck"
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

// Validate checks the live catalog (no files) or the catalog as it would be
// with files applied. Nothing is stored or written.
func (cc *CatalogApplyClient) Validate(ctx context.Context, files []catalogapply.File) (*catalogcheck.Report, error) {
	var out catalogcheck.Report
	if err := cc.do(ctx, http.MethodPost, "/catalog/validate", api.CatalogValidateRequest{Files: files}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Files lists the catalog files an agent may read raw.
func (cc *CatalogApplyClient) Files(ctx context.Context) ([]catalogapply.FileInfo, error) {
	var out struct {
		Files []catalogapply.FileInfo `json:"files"`
	}
	if err := cc.do(ctx, http.MethodGet, "/catalog/files", nil, &out); err != nil {
		return nil, err
	}
	return out.Files, nil
}

// ReadFile returns one catalog file's raw content.
func (cc *CatalogApplyClient) ReadFile(ctx context.Context, path string) (string, error) {
	var out struct {
		Content string `json:"content"`
	}
	if err := cc.do(ctx, http.MethodGet, "/catalog/files?path="+url.QueryEscape(path), nil, &out); err != nil {
		return "", err
	}
	return out.Content, nil
}

// History lists applied catalog changes, newest first.
func (cc *CatalogApplyClient) History(ctx context.Context, limit int) ([]catalogapply.AuditRecord, error) {
	var out struct {
		Changes []catalogapply.AuditRecord `json:"changes"`
	}
	if err := cc.do(ctx, http.MethodGet, "/catalog/audit?limit="+strconv.Itoa(limit), nil, &out); err != nil {
		return nil, err
	}
	return out.Changes, nil
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
