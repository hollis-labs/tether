package api

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/hollis-labs/tether/internal/authissuer"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/session"
	"github.com/hollis-labs/tether/internal/store"
)

// ServiceTokenRequestPath is where a session asks for a service token for its own
// app (CW-20261020-0036). It is local-listener only; the remote listener refuses it.
const ServiceTokenRequestPath = ServiceTokensPath + "/request"

// Service request modes (identity.service_request in global.yaml).
const (
	// ServiceRequestAuto issues the token at once and logs it (dev posture).
	ServiceRequestAuto = "auto"
	// ServiceRequestApproval records the request and answers 202; issuing waits for
	// the approval flow (CW-20261015-0052). It is the default.
	ServiceRequestApproval = "approval"
)

// SessionLookup loads the session row the hub trusts. The request names no session:
// the hub loads the row of the verified caller's own session id.
type SessionLookup interface {
	GetSession(id string) (*store.SessionRow, error)
}

// WithServiceRequests enables POST /auth/service-tokens/request. mode is "auto" or
// anything else for approval (the default), so a config that omits it never
// auto-approves.
func WithServiceRequests(lookup SessionLookup, mode string) IssuerOption {
	return func(c *issuerConfig) {
		c.sessionLookup = lookup
		c.requestMode = mode
	}
}

// ServiceTokenSessionRequest is the body of POST /auth/service-tokens/request. There
// is deliberately no app, env or audience: the app is the session's own, stamped when
// the operator launched it, and an unknown field is refused.
type ServiceTokenSessionRequest struct {
	Scopes []string `json:"scopes,omitempty"`
	Label  string   `json:"label,omitempty"`
}

// ServiceTokenPending is the 202 answer in approval mode.
type ServiceTokenPending struct {
	Status  string `json:"status"`
	App     string `json:"app"`
	Message string `json:"message"`
}

func serviceTokenRequestHandler(is *authissuer.Issuer, cfg issuerConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := identity.FromContext(r.Context())
		if !ok || !p.Verified {
			// Unverified or anonymous callers are not audited: identity/guard.go forbids
			// unauthenticated traffic from allocating audit rows, and the same policy applies
			// to the task_id and app stamps. Only a verified caller leaves a row.
			writeError(w, http.StatusUnauthorized, "unauthorized", "verified caller identity required")
			return
		}
		audit := func(app, decision, jti string, scopes []string) {
			if cfg.mintAudit == nil {
				return
			}
			event := fmt.Sprintf("service_token_request app=%s decision=%s", app, decision)
			if jti != "" {
				event += " jti=" + jtiPrefix(jti)
			}
			rec := identity.Observation{
				At: time.Now().UTC(), PrincipalID: p.ID, SessionID: p.SessionID, Mode: "issuer",
				Authentication: "verified", Method: http.MethodPost, Route: ServiceTokenRequestPath, RoutePattern: ServiceTokenRequestPath,
				RequiredScope: strings.Join(scopes, ","), Event: event,
			}
			if err := cfg.mintAudit(r.Context(), rec); err != nil {
				log.Printf("authissuer: service token request audit row not recorded: %v", err)
			}
		}
		// Only a session or agent speaking for its own session; never the operator, a
		// device or a service, and never a remote caller.
		if (p.Kind != "agent" && p.Kind != "session") || p.SessionID == "" || identity.IsRemote(r.Context()) || !identity.LocalConnection(r.Context()) {
			audit("", "refused_caller", "", nil)
			writeError(w, http.StatusForbidden, CodeForbidden, "only a session asks for its own app's service token, over the local socket")
			return
		}
		var req ServiceTokenSessionRequest
		if !decodeDeviceBody(w, r, &req) {
			// A verified caller's malformed or unknown-field body is audited as invalid;
			// the row carries no body content.
			audit("", "refused_invalid", "", nil)
			return
		}
		row, err := cfg.sessionLookup.GetSession(p.SessionID)
		if err != nil || row == nil {
			audit("", "refused_unknown_session", "", req.Scopes)
			writeError(w, http.StatusForbidden, CodeForbidden, "the session is not known to the hub")
			return
		}
		caller := authissuer.SessionCaller{
			PrincipalID: p.ID, SessionID: p.SessionID, App: row.App.String,
			Ended: row.EndedAt.Valid || session.State(row.State).Terminal(),
		}
		app := caller.App
		if cfg.requestMode != ServiceRequestAuto {
			// Validate what would be issued, then record the request without minting.
			if err := precheckSessionRequest(caller, req); err != nil {
				audit(app, "refused", "", req.Scopes)
				writeIssuerError(w, err)
				return
			}
			audit(app, "pending", "", req.Scopes)
			log.Printf("authissuer: service token request pending app=%s sid=%s by=%s", app, p.SessionID, p.ID)
			w.Header().Set("Cache-Control", "no-store")
			writeJSON(w, http.StatusAccepted, ServiceTokenPending{Status: "pending", App: app, Message: "service token requests need approval; none was issued"})
			return
		}
		tok, rec, err := is.RequestService(caller, authissuer.SessionServiceRequest{Scopes: req.Scopes, Label: req.Label})
		if err != nil {
			audit(app, "refused", "", req.Scopes)
			writeIssuerError(w, err)
			return
		}
		audit(app, "issued", rec.JTI, rec.Scopes)
		// Ids and scopes only: the token is never logged.
		log.Printf("authissuer: issued service token on request jti=%s sub=%s scopes=%q sid=%s by=%s", rec.JTI, rec.Sub, rec.Scopes, p.SessionID, p.ID)
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, ServiceTokenResponse{Token: tok, Record: rec})
	}
}

// precheckSessionRequest applies the request rules without minting, so approval mode
// refuses what auto mode would refuse before queuing it.
func precheckSessionRequest(c authissuer.SessionCaller, req ServiceTokenSessionRequest) error {
	return authissuer.CheckSessionRequest(c, authissuer.SessionServiceRequest{Scopes: req.Scopes, Label: req.Label})
}

func writeIssuerError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, authissuer.ErrForbidden):
		writeError(w, http.StatusForbidden, CodeForbidden, strings.TrimPrefix(err.Error(), "authissuer: "))
	case errors.Is(err, authissuer.ErrInvalid):
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, strings.TrimPrefix(err.Error(), "authissuer: "))
	default:
		writeError(w, http.StatusInternalServerError, CodeInternalError, "service token could not be issued")
	}
}

func jtiPrefix(jti string) string {
	if len(jti) > 8 {
		return jti[:8]
	}
	return jti
}
