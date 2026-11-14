package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

// Address aliases (CW-20261114-0067). The daemon applies every rule; these
// methods only carry the JSON.

// AddressAliasList lists every alias, or only those of canonical when set.
func (c *Client) AddressAliasList(ctx context.Context, canonical string) (json.RawMessage, error) {
	path := "/messages/aliases"
	if canonical != "" {
		path += "?canonical=" + url.QueryEscape(canonical)
	}
	var out json.RawMessage
	if err := c.getJSON(ctx, path, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// AddressAliasAdd declares alias another name for canonical (operator only).
func (c *Client) AddressAliasAdd(ctx context.Context, alias, canonical string) (json.RawMessage, error) {
	body, err := json.Marshal(map[string]string{"alias": alias, "canonical": canonical})
	if err != nil {
		return nil, err
	}
	return c.postOK(ctx, "/messages/aliases", body)
}

// AddressAliasRemove removes one alias (operator only).
func (c *Client) AddressAliasRemove(ctx context.Context, alias string) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.baseURL+"/messages/aliases?alias="+url.QueryEscape(alias), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, wrapIfUnreachable(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, readError(resp)
	}
	var out json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode alias remove response: %w", err)
	}
	return out, nil
}
