package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/teamsvc"
)

// TeamRoster reads from the owning daemon; no local database or identity claim
// is consulted, and no idempotency key or delivery acknowledgement is generated.
func (c *Client) TeamRoster(ctx context.Context, req teamsvc.RosterRequest) (teamsvc.RosterList, error) {
	if teamsvc.ValidateRosterRequest(req) != nil {
		return teamsvc.RosterList{}, &TeamError{Code: "invalid_request", Status: 400}
	}
	query := url.Values{}
	if req.RunID != "" {
		query.Set("run_id", req.RunID)
	}
	if req.After != "" {
		query.Set("after", req.After)
	}
	if req.Limit != 0 {
		query.Set("limit", strconv.Itoa(req.Limit))
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/teams/roster?"+query.Encode(), nil)
	if err != nil {
		return teamsvc.RosterList{}, &TeamError{Code: "invalid_request", Status: 400}
	}
	response, err := c.http.Do(request)
	if err != nil {
		return teamsvc.RosterList{}, &TeamError{Code: "unavailable", Status: 503}
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, teamsvc.MaxRequestBytes+1))
	if err != nil || len(body) > teamsvc.MaxRequestBytes {
		return teamsvc.RosterList{}, &TeamError{Code: "internal_error", Status: 500}
	}
	if response.StatusCode != http.StatusOK {
		var envelope api.ErrorResponse
		if response.StatusCode == 404 && json.Unmarshal(body, &envelope) != nil {
			return teamsvc.RosterList{}, &TeamError{Code: "teams_disabled", Status: 404}
		}
		code, status := "internal_error", 500
		switch response.StatusCode {
		case 400:
			code, status = "invalid_request", 400
		case 401:
			code, status = "unauthenticated", 401
		case 403:
			code, status = "denied", 403
		case 404:
			code, status = "not_found", 404
		case 405:
			code, status = "method_not_allowed", 405
		case 409:
			code, status = "conflict", 409
		default:
			if response.StatusCode > 500 {
				code, status = "unavailable", 503
			}
		}
		return teamsvc.RosterList{}, &TeamError{Code: code, Status: status}
	}
	var result teamsvc.RosterList
	if json.Unmarshal(body, &result) != nil {
		return teamsvc.RosterList{}, &TeamError{Code: "internal_error", Status: 500}
	}
	return result, nil
}
