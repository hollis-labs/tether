package api

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/hollis-labs/tether/internal/authissuer"
	"github.com/hollis-labs/tether/internal/identity"
)

// Paths served by the token issuer handler.
const (
	TokenPath    = "/auth/token" //nolint:gosec // G101: a route path, not a credential
	DenyListPath = "/auth/denylist"
	JWKSPath     = "/.well-known/jwks.json"
)

// TokenRequest is the body of POST /auth/token.
type TokenRequest struct {
	Audience   string   `json:"audience"`
	Scopes     []string `json:"scopes"`
	TTLSeconds int      `json:"ttl_seconds,omitempty"`
	SessionID  string   `json:"session_id,omitempty"`
}

type issuerConfig struct {
	mintAudit     func(context.Context, identity.Observation) error
	workloadStore *identity.Store
}

// IssuerOption configures NewTokenIssuerHandler.
type IssuerOption func(*issuerConfig)

// WithMintAudit records an identity audit row for every session-bound mint.
func WithMintAudit(record func(context.Context, identity.Observation) error) IssuerOption {
	return func(c *issuerConfig) { c.mintAudit = record }
}

// ServiceTokensPath is the operator-only service token surface (CW-20261019-0044).
const ServiceTokensPath = "/auth/service-tokens" //nolint:gosec // G101: a route path, not a credential

// ServiceTokenRequest is the body of POST /auth/service-tokens.
type ServiceTokenRequest struct {
	App           string   `json:"app"`
	Env           string   `json:"env,omitempty"`
	Audience      string   `json:"audience"`
	Scopes        []string `json:"scopes"`
	Label         string   `json:"label,omitempty"`
	AllowWildcard bool     `json:"allow_wildcard,omitempty"`
}

// ServiceTokenResponse is the answer to POST /auth/service-tokens: the token,
// shown once, and the metadata the issuer keeps (never the token).
type ServiceTokenResponse struct {
	authissuer.Token
	Record authissuer.ServiceRecord `json:"record"`
}

// ServiceTokenListResponse is the answer to GET /auth/service-tokens.
type ServiceTokenListResponse struct {
	Tokens []authissuer.ServiceRecord `json:"tokens"`
}

// DenyKindParent is the DenyRequest kind that denies every unexpired token
// minted from the named parent principal (see authissuer.State.DenyMintedBy).
const DenyKindParent = "par"

// DenyRequest is the body of POST /auth/denylist.
type DenyRequest struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

// NewTokenIssuerHandler serves the issuer surface: the JWKS (public), the
// token exchange, and the deny-list verifiers poll. The exchange and the
// deny-list use Tether's own bearer auth; issued tokens (agent tokens in
// particular) can never mint.
func NewTokenIssuerHandler(is *authissuer.Issuer, opts ...IssuerOption) http.Handler {
	var cfg issuerConfig
	for _, o := range opts {
		o(&cfg)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /auth/workload-trust", workloadTrustHandler(is, cfg.workloadStore))
	mux.HandleFunc("GET /.well-known/jwks.json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "public, max-age=60")
		_, _ = w.Write(is.Keys.JWKS())
	})
	mux.HandleFunc("POST /auth/token", func(w http.ResponseWriter, r *http.Request) {
		p, ok := identity.FromContext(r.Context())
		if !ok || !p.Verified {
			writeError(w, http.StatusUnauthorized, "unauthorized", "verified caller identity required")
			return
		}
		// The operator credential is trusted only over the local socket.
		if p.Kind == "operator" && !localOperatorPrincipal(r, p) {
			writeError(w, http.StatusForbidden, CodeForbidden, "the operator credential mints only over the local socket")
			return
		}
		var req TokenRequest
		if !decodeDeviceBody(w, r, &req) {
			return
		}
		tok, err := is.Exchange(p, authissuer.Request{Audience: req.Audience, Scopes: req.Scopes, TTL: time.Duration(req.TTLSeconds) * time.Second, SessionID: req.SessionID})
		switch {
		case errors.Is(err, authissuer.ErrWorkloadMintLimit):
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Retry-After", "60")
			writeError(w, http.StatusTooManyRequests, "workload_mint_limit", "reuse the current status token or retry after mint capacity expires")
			return
		case errors.Is(err, authissuer.ErrForbidden):
			writeError(w, http.StatusForbidden, CodeForbidden, strings.TrimPrefix(err.Error(), "authissuer: "))
			return
		case errors.Is(err, authissuer.ErrInvalid):
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, strings.TrimPrefix(err.Error(), "authissuer: "))
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, CodeInternalError, "token could not be issued")
			return
		}
		if req.SessionID != "" && cfg.mintAudit != nil {
			// One audit row per session-bound mint: who minted (par), for which
			// session, audience, scopes, token id and expiry. Never the token.
			rec := identity.Observation{
				At: time.Now().UTC(), PrincipalID: p.ID, SessionID: req.SessionID, Mode: "issuer",
				Authentication: "verified", Method: http.MethodPost, Route: TokenPath, RoutePattern: TokenPath,
				RequiredScope: strings.Join(tok.Scopes, ","),
				Event:         fmt.Sprintf("token_mint aud=%s jti=%s exp=%d", tok.Audience, tok.JTI, tok.ExpiresAt.Unix()),
			}
			if err := cfg.mintAudit(r.Context(), rec); err != nil {
				log.Printf("authissuer: mint audit row not recorded: %v", err)
			}
		}
		log.Printf("authissuer: issued kind=%s aud=%s sub=%s par=%s sid=%s scopes=%q ttl=%ds", tok.Kind, tok.Audience, subOf(p, req.SessionID), p.ID, req.SessionID, tok.Scopes, tok.TTL)
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, tok)
	})
	mux.HandleFunc("POST /auth/service-tokens", func(w http.ResponseWriter, r *http.Request) {
		p, ok := serviceTokenOperator(w, r)
		if !ok {
			return
		}
		var req ServiceTokenRequest
		if !decodeDeviceBody(w, r, &req) {
			return
		}
		tok, rec, err := is.IssueService(p, authissuer.ServiceRequest{App: req.App, Env: req.Env, Audience: req.Audience, Scopes: req.Scopes, Label: req.Label, AllowWildcard: req.AllowWildcard})
		switch {
		case errors.Is(err, authissuer.ErrForbidden):
			writeError(w, http.StatusForbidden, CodeForbidden, strings.TrimPrefix(err.Error(), "authissuer: "))
			return
		case errors.Is(err, authissuer.ErrInvalid):
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, strings.TrimPrefix(err.Error(), "authissuer: "))
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, CodeInternalError, "service token could not be issued")
			return
		}
		// Ids and scopes only: the token is never logged.
		log.Printf("authissuer: issued service token jti=%s sub=%s aud=%s scopes=%q by=%s", rec.JTI, rec.Sub, rec.Audience, rec.Scopes, rec.IssuedBy)
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, ServiceTokenResponse{Token: tok, Record: rec})
	})
	mux.HandleFunc("GET /auth/service-tokens", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := serviceTokenOperator(w, r); !ok {
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, ServiceTokenListResponse{Tokens: is.State.Services()})
	})
	mux.HandleFunc("GET /auth/denylist", func(w http.ResponseWriter, r *http.Request) {
		if !denyListReader(r, false) {
			writeError(w, http.StatusForbidden, "scope_required", "required scope: maintain")
			return
		}
		body, etag := is.State.DenyDoc()
		w.Header().Set("ETag", etag)
		w.Header().Set("Cache-Control", "no-cache")
		if match := r.Header.Get("If-None-Match"); match != "" && match == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	})
	mux.HandleFunc("POST /auth/denylist", func(w http.ResponseWriter, r *http.Request) {
		if !denyListReader(r, true) {
			writeError(w, http.StatusForbidden, "scope_required", "required scope: admin")
			return
		}
		var req DenyRequest
		if !decodeDeviceBody(w, r, &req) {
			return
		}
		if req.Kind == DenyKindParent {
			// Deny every unexpired token still tracked as minted from this
			// parent: what rotating the operator credential needs.
			n, err := is.State.DenyMintedBy(req.Value)
			if err != nil {
				writeError(w, http.StatusBadRequest, CodeInvalidRequest, "value must name a parent principal")
				return
			}
			log.Printf("authissuer: denied %d unexpired tokens minted from %s", n, req.Value)
			writeJSON(w, http.StatusOK, map[string]any{"denied": true, "tokens": n})
			return
		}
		until := time.Now().Add(authissuer.DenyHorizon)
		if req.Kind == authissuer.DenySub {
			until = time.Time{} // a revoked principal stays revoked
		}
		if err := is.State.Deny(req.Kind, req.Value, until); err != nil {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, "kind must be jti, sub or sid, with a value")
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"denied": true})
	})
	return mux
}

// serviceTokenOperator admits only the verified operator credential on the
// local socket; every other caller, a remote one, a device, a service or an
// agent, is refused. It writes the refusal.
func serviceTokenOperator(w http.ResponseWriter, r *http.Request) (identity.Principal, bool) {
	p, ok := identity.FromContext(r.Context())
	if !ok || !p.Verified {
		writeError(w, http.StatusUnauthorized, "unauthorized", "verified caller identity required")
		return p, false
	}
	if p.Kind != "operator" || !identity.IsOperatorCredential(p) || !localOperatorPrincipal(r, p) {
		writeError(w, http.StatusForbidden, CodeForbidden, "service tokens are managed only by the operator over the local socket")
		return p, false
	}
	return p, true
}

func subOf(parent identity.Principal, sid string) string {
	if sid != "" {
		return authissuer.SessionPrincipalID(sid)
	}
	return parent.ID
}

func localOperatorPrincipal(r *http.Request, p identity.Principal) bool {
	return !identity.IsRemote(r.Context()) && identity.LocalConnection(r.Context()) && p.ID == identity.OperatorID
}

// denyListReader: the deny-list uses Tether's own auth, admin/maintain grade.
// The local operator, a stored credential holding maintain or admin (write
// needs admin), or an issued non-agent token holding the tether.maintain or
// tether.admin scope may use it. Agent tokens never can.
func denyListReader(r *http.Request, write bool) bool {
	p, ok := identity.FromContext(r.Context())
	if !ok || !p.Verified || p.Kind == "agent" {
		return false
	}
	if p.Kind == "operator" {
		return identity.IsOperatorCredential(p) && localOperatorPrincipal(r, p)
	}
	if write {
		return identity.ScopeGranted(p.Scopes, identity.ScopeAdmin)
	}
	return identity.ScopeGranted(p.Scopes, identity.ScopeMaintain) || identity.ScopeGranted(p.Scopes, identity.ScopeAdmin)
}
