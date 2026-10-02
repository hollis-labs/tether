package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/hollis-labs/tether/internal/app/routingcap"
)

type RoutingService interface {
	RoutingCapabilities(context.Context, string) (routingcap.RoutingCapabilitiesResponse, error)
}

func (s *Server) registerRoutingRoutes(router *http.ServeMux) {
	if s.Routing == nil {
		return
	}
	router.HandleFunc("/routing/capabilities", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
			return
		}
		result, err := s.Routing.RoutingCapabilities(r.Context(), r.URL.Query().Get("session_id"))
		if errors.Is(err, routingcap.ErrSessionNotFound) {
			writeError(w, http.StatusNotFound, CodeNotFound, err.Error())
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, result)
	})
}
