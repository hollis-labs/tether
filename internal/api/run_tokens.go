package api

import (
	"context"
	"github.com/hollis-labs/tether/internal/store"
	"net/http"
)

const RunTokenRevokePath = "/auth/run-tokens/revoke" //nolint:gosec // G101: route path, not a credential.

type RunTokenRevokeRequest struct {
	Target string `json:"target"`
}
type RunTokenRevoker interface {
	RevokeRunToken(context.Context, string) error
}

func (s *Server) registerRunTokenRoutes(router *http.ServeMux) {
	revoker, ok := s.Service.(RunTokenRevoker)
	if !ok {
		return
	}
	router.HandleFunc("POST /auth/run-tokens/revoke", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := serviceTokenOperator(w, r); !ok {
			return
		}
		var req RunTokenRevokeRequest
		if !decodeDeviceBody(w, r, &req) {
			return
		}
		if !store.ValidRunTokenTarget(req.Target) {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, "expected an exact jti or execution run identifier")
			return
		}
		if err := revoker.RevokeRunToken(r.Context(), req.Target); err != nil {
			writeError(w, http.StatusInternalServerError, CodeInternalError, "run token revocation failed")
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, req)
	})
}
