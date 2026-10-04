package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/teamsvc"
)

// TeamError is a cause-free transport error shared by the CLI and remote tools.
type TeamError struct {
	Code   string
	Status int
}

func (e *TeamError) Error() string {
	if e.Code == "teams_disabled" {
		return "teams are disabled"
	}
	return e.Code
}

// Team invokes a team verb on the owning daemon using the client's credential.
// No caller attribution is taken from the request or synthesized locally.
func (c *Client) Team(ctx context.Context, verb, key string, req api.TeamRequest) (teamsvc.Result, error) {
	var result teamsvc.Result
	var body bytes.Buffer
	enc := json.NewEncoder(&body)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(req); err != nil {
		return result, &TeamError{Code: "invalid_request", Status: 400}
	}
	known := false
	for _, v := range api.TeamVerbs() {
		if verb == v {
			known = true
		}
	}
	if !known || len(body.Bytes()) > teamsvc.MaxRequestBytes || api.ValidateTeamKey(key) != nil {
		return result, &TeamError{Code: "invalid_request", Status: 400}
	}
	if _, err := api.DecodeTeamRequest(verb, key, bytes.NewReader(body.Bytes())); err != nil {
		return result, &TeamError{Code: "invalid_request", Status: 400}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/teams/"+verb, &body)
	if err != nil {
		return result, &TeamError{Code: "internal_error", Status: 500}
	}
	request.Header.Set("Idempotency-Key", key)
	request.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return result, &TeamError{Code: "unavailable", Status: 503}
	}
	defer func() { _ = response.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(response.Body, teamsvc.MaxRequestBytes+1))
	if err != nil || len(b) > teamsvc.MaxRequestBytes {
		return result, &TeamError{Code: "internal_error", Status: 500}
	}
	if response.StatusCode != http.StatusOK {
		var envelope api.ErrorResponse
		if response.StatusCode == 404 && json.Unmarshal(b, &envelope) != nil {
			return result, &TeamError{Code: "teams_disabled", Status: 404}
		}
		_ = json.Unmarshal(b, &envelope)
		for _, cause := range []error{teamsvc.ErrUnauthenticated, teamsvc.ErrUnavailable, teamsvc.ErrDenied, teamsvc.ErrNotFound, teamsvc.ErrConflict, teamsvc.ErrInvalidRequest, errors.New("unknown")} {
			status, detail := api.TeamError(cause)
			if envelope.Error.Code == detail.Code && response.StatusCode == status {
				return result, &TeamError{Code: detail.Code, Status: status}
			}
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
			if response.StatusCode >= 500 {
				code, status = "unavailable", 503
			}
		}
		return result, &TeamError{Code: code, Status: status}
	}
	if json.Unmarshal(b, &result) != nil {
		return result, &TeamError{Code: "internal_error", Status: 500}
	}
	return result, nil
}
