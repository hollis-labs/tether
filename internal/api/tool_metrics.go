package api

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/hollis-labs/tether/internal/telemetry"
)

func (s *Server) registerToolMetricsRoute(router *http.ServeMux) {
	source, ok := s.EventsStore.(telemetry.MetricsSource)
	if !ok {
		return
	}
	router.HandleFunc("/events/tool-metrics", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
			return
		}
		q := r.URL.Query()
		req := telemetry.MetricsRequest{Tool: q.Get("tool"), Upstream: q.Get("upstream"), Since: q.Get("since"), Until: q.Get("until"), Mode: q.Get("mode"), GroupBy: q.Get("group_by"), Compact: q.Get("compact") == "true"}
		if raw := q.Get("limit"); raw != "" {
			n, convErr := strconv.Atoi(raw)
			if convErr != nil {
				writeError(w, http.StatusBadRequest, CodeInvalidRequest, "limit must be an integer")
				return
			}
			req.Limit = n
		}
		out, err := telemetry.QueryService{Source: source}.ReadMetrics(r.Context(), req)
		if err != nil {
			var invalid *telemetry.QueryError
			if errors.As(err, &invalid) {
				writeError(w, http.StatusBadRequest, CodeInvalidRequest, err.Error())
			} else {
				writeError(w, http.StatusInternalServerError, CodeInternalError, "query tool metrics: "+err.Error())
			}
			return
		}
		writeJSON(w, http.StatusOK, out)
	})
}
