package api

import (
	"encoding/base64"
	"errors"
	"net/http"
	"time"

	auth "github.com/hollis-labs/libs/security/auth"
	"github.com/hollis-labs/tether/internal/authissuer"
	"github.com/hollis-labs/tether/internal/identity"
)

const WorkloadTrustPath = "/auth/workload-trust"

// WithWorkloadTrustStore enables credential-bound lifecycle reads. Without the
// daemon's authoritative identity store the route always fails closed.
func WithWorkloadTrustStore(ids *identity.Store) IssuerOption {
	return func(c *issuerConfig) { c.workloadStore = ids }
}

type WorkloadTrustRequest struct {
	QueryID  string                   `json:"query_id"`
	Workload authissuer.WorkloadQuery `json:"workload"`
}

type WorkloadTrustResponse struct {
	QueryID   string                   `json:"query_id"`
	Workload  authissuer.WorkloadQuery `json:"workload"`
	Active    bool                     `json:"active"`
	CheckedAt time.Time                `json:"checked_at"`
}

func workloadTrustHandler(is *authissuer.Issuer, ids *identity.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if ids == nil || is == nil || is.State == nil {
			writeError(w, 503, "current_trust_unavailable", "current workload trust unavailable")
			return
		}
		// Reauthenticate the stored credential here, even in local off/observe
		// mode. Issued JWTs, operators and wildcard maintenance grants cannot
		// become this independent read capability.
		bearer, state := auth.ParseBearer(r.Header.Values("Authorization"))
		if state != "" {
			writeError(w, 401, "unauthorized", "workload trust reader required")
			return
		}
		p, err := ids.Verify(r.Context(), bearer)
		if err != nil && !errors.Is(err, identity.ErrInvalidToken) {
			writeError(w, 503, "current_trust_unavailable", "current workload trust unavailable")
			return
		}
		if err != nil || !p.Verified || p.Kind != "device" || len(p.Scopes) != 1 ||
			p.Scopes[0] != identity.WorkloadTrustScope || is.State.Denied(authissuer.DenySub, p.ID) {
			writeError(w, 403, CodeForbidden, "dedicated workload trust reader required")
			return
		}
		var req WorkloadTrustRequest
		if !decodeDeviceBody(w, r, &req) {
			return
		}
		nonce, err := base64.RawURLEncoding.DecodeString(req.QueryID)
		q := req.Workload
		if err != nil || len(nonce) != 32 || base64.RawURLEncoding.EncodeToString(nonce) != req.QueryID ||
			q.Issuer != is.Name || !identity.WorkloadScopesAllowed(q.Audience, q.Scopes) ||
			(q.Kind != "device" && q.Kind != "service") || q.Subject == "" || q.Parent != q.Subject ||
			q.TokenID == "" || q.ExpiresAt.IsZero() {
			writeError(w, 400, CodeInvalidRequest, "invalid current workload query")
			return
		}
		active, err := is.CurrentWorkload(r.Context(), ids, q)
		if err != nil {
			writeError(w, 503, "current_trust_unavailable", "current workload trust unavailable")
			return
		}
		writeJSON(w, 200, WorkloadTrustResponse{QueryID: req.QueryID, Workload: q, Active: active, CheckedAt: is.Now().UTC()})
	}
}
