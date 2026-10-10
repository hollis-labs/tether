package api

import (
	"context"
	"net/http"
	"net/url"
	"strconv"

	"github.com/hollis-labs/tether/internal/teamsvc"
)

// TeamRosterOps is additive: old hosts do not gain a route or read authority.
type TeamRosterOps interface {
	ListRoster(context.Context, teamsvc.RosterRequest) (teamsvc.RosterList, error)
}

var _ TeamRosterOps = (*teamsvc.Service)(nil)

func (s *Server) registerTeamRosterRoute(mux *http.ServeMux) {
	ops, ok := s.Teams.(TeamRosterOps)
	if !ok {
		return
	}
	mux.HandleFunc("/teams/roster", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeError(w, 405, CodeMethodNotAllowed, "method not allowed")
			return
		}
		query, err := parseRosterRequest(r)
		if err != nil {
			writeError(w, 400, CodeInvalidRequest, "invalid request")
			return
		}
		result, err := ops.ListRoster(r.Context(), query)
		if err != nil {
			status, detail := TeamError(err)
			writeError(w, status, detail.Code, detail.Message)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, 200, result)
	})
}

// parseRosterRequest refuses unknown, duplicate and malformed query selectors.
func parseRosterRequest(r *http.Request) (teamsvc.RosterRequest, error) {
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return teamsvc.RosterRequest{}, teamsvc.ErrInvalidRequest
	}
	for key, entries := range values {
		if len(entries) != 1 || key != "run_id" && key != "after" && key != "limit" {
			return teamsvc.RosterRequest{}, teamsvc.ErrInvalidRequest
		}
	}
	req := teamsvc.RosterRequest{RunID: values.Get("run_id"), After: values.Get("after")}
	if raw, present := values["limit"]; present {
		req.Limit, err = strconv.Atoi(raw[0])
		if err != nil || req.Limit < 1 {
			return teamsvc.RosterRequest{}, teamsvc.ErrInvalidRequest
		}
	}
	return req, teamsvc.ValidateRosterRequest(req)
}
