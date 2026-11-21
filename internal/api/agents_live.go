package api

import (
	"context"
	"net/http"
	"strconv"

	"github.com/hollis-labs/tether/internal/agentlive"
)

// AgentsLiveService is the optional read surface behind GET /agents/live. The
// tracker carries open mail-watch presence; nil means no watcher evidence.
type AgentsLiveService interface {
	AgentsLive(ctx context.Context, q agentlive.Query, watch *agentlive.Tracker) (agentlive.Response, error)
}

// AgentsLiveResponse is the GET /agents/live body.
type AgentsLiveResponse struct {
	agentlive.Response
}

func (s *Server) registerAgentsLiveRoutes(router *http.ServeMux) {
	svc, ok := s.Service.(AgentsLiveService)
	if !ok {
		return
	}
	router.HandleFunc("/agents/live", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
			return
		}
		q, err := ParseAgentsLiveQuery(r.URL.Query().Get)
		if err != nil {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, err.Error())
			return
		}
		out, err := svc.AgentsLive(r.Context(), q, s.Presence)
		if err != nil {
			writeError(w, http.StatusInternalServerError, CodeInternalError, "agents live unavailable")
			return
		}
		writeJSON(w, http.StatusOK, out)
	})
}

// ParseAgentsLiveQuery turns raw filter strings (query params) into a Query;
// the MCP tool and the HTTP route share it so both validate identically.
func ParseAgentsLiveQuery(get func(string) string) (agentlive.Query, error) {
	include := false
	if v := get("include_stale"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return agentlive.Query{}, errInvalid("invalid include_stale: want true or false")
		}
		include = b
	}
	num := func(name string) (int, error) {
		v := get(name)
		if v == "" {
			return 0, nil
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			return 0, errInvalid("invalid " + name + ": want an integer")
		}
		return n, nil
	}
	stale, err := num("stale_after_seconds")
	if err != nil {
		return agentlive.Query{}, err
	}
	limit, err := num("limit")
	if err != nil {
		return agentlive.Query{}, err
	}
	return agentlive.ParseQuery(get("host"), get("kind"), include, stale, limit, get("cursor"))
}

type errInvalid string

func (e errInvalid) Error() string { return string(e) }
