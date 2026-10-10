package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/hollis-labs/tether/internal/identity"
)

type PairRequest struct {
	Label         string   `json:"label"`
	Scopes        []string `json:"scopes"`
	TTL           string   `json:"ttl,omitempty"`
	KeyThumbprint string   `json:"key_thumbprint,omitempty"`
}

type PairExchangeRequest struct {
	Code          string   `json:"code"`
	Scopes        []string `json:"scopes"`
	KeyThumbprint string   `json:"key_thumbprint,omitempty"`
}

type DeviceRevokeRequest struct {
	ID string `json:"id"`
}
type DeviceRenewRequest struct {
	Scopes []string `json:"scopes"`
}

type pairingLimiter struct {
	mu     sync.Mutex
	tokens float64
	last   time.Time
}

func (l *pairingLimiter) allow(now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.last.IsZero() {
		l.tokens, l.last = 10, now
	}
	l.tokens = min(10, l.tokens+max(0, now.Sub(l.last).Seconds())/6)
	l.last = now
	if l.tokens < 1 {
		return false
	}
	l.tokens--
	return true
}

// NewDeviceAuthHandler owns only identity management, never provider credentials,
// sessions or live shim custody. Its global bounded limiter intentionally does
// not trust forwarded IP headers or allocate attacker-selected bucket keys.
func NewDeviceAuthHandler(store *identity.Store) http.Handler {
	limiter := &pairingLimiter{}
	router := http.NewServeMux()
	router.HandleFunc("POST /auth/pair", func(w http.ResponseWriter, r *http.Request) {
		p, ok := localOperator(r)
		if !ok {
			writeError(w, http.StatusForbidden, CodeForbidden, "pairing requires the local Unix socket and operator credential")
			return
		}
		var req PairRequest
		if !decodeDeviceBody(w, r, &req) {
			return
		}
		ttl := identity.DefaultGrantTTL
		if req.TTL != "" {
			var err error
			ttl, err = time.ParseDuration(req.TTL)
			if err != nil || ttl <= 0 || ttl > identity.MaxGrantTTL {
				writeError(w, 400, CodeInvalidRequest, "ttl must be positive and at most one hour")
				return
			}
		}
		if req.Scopes == nil {
			req.Scopes = []string{identity.ScopeRead}
		}
		grant, err := store.CreatePairingGrant(r.Context(), p, req.Label, req.Scopes, ttl, req.KeyThumbprint)
		if err != nil {
			writeError(w, 400, CodeInvalidRequest, "pairing grant could not be created")
			return
		}
		writeJSON(w, http.StatusCreated, grant)
	})
	router.HandleFunc("POST /auth/pair/revoke", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := localOperator(r); !ok {
			writeError(w, 403, CodeForbidden, "local operator required")
			return
		}
		var req DeviceRevokeRequest
		if !decodeDeviceBody(w, r, &req) {
			return
		}
		if err := store.RevokePairingGrant(r.Context(), req.ID); err != nil {
			writeError(w, 503, CodeInternalError, "pairing revocation unavailable")
			return
		}
		writeJSON(w, 200, map[string]bool{"revoked": true})
	})
	router.HandleFunc("POST /auth/pair/exchange", func(w http.ResponseWriter, r *http.Request) {
		if !identity.IsRemote(r.Context()) {
			writeError(w, 403, CodeForbidden, "exchange requires the remote listener")
			return
		}
		if !limiter.allow(time.Now()) {
			w.Header().Set("Retry-After", "6")
			writeError(w, 429, "pairing_rate_limited", "pairing exchange rate limit reached")
			return
		}
		var req PairExchangeRequest
		if !decodeDeviceBody(w, r, &req) {
			return
		}
		result, err := store.ExchangePairingGrant(r.Context(), req.Code, req.Scopes, req.KeyThumbprint)
		if errors.Is(err, identity.ErrInvalidGrant) {
			writeError(w, 401, "pairing_refused", "pairing grant unavailable")
			return
		}
		if err != nil {
			writeError(w, 503, CodeInternalError, "pairing exchange unavailable")
			return
		}
		writeJSON(w, 200, result)
	})
	router.HandleFunc("GET /auth/devices", func(w http.ResponseWriter, r *http.Request) {
		if !deviceAdmin(r) {
			writeError(w, 403, CodeForbidden, "device administration required")
			return
		}
		devices, err := store.ListDevices(r.Context())
		if err != nil {
			writeError(w, 503, CodeInternalError, "device list unavailable")
			return
		}
		writeJSON(w, 200, map[string]any{"devices": devices})
	})
	router.HandleFunc("POST /auth/revoke", func(w http.ResponseWriter, r *http.Request) {
		if !deviceAdmin(r) {
			writeError(w, 403, CodeForbidden, "device administration required")
			return
		}
		var req DeviceRevokeRequest
		if !decodeDeviceBody(w, r, &req) {
			return
		}
		err := store.RevokeDevice(r.Context(), req.ID)
		if errors.Is(err, identity.ErrDeviceNotFound) {
			writeError(w, 404, CodeNotFound, "device not found")
			return
		}
		if err != nil {
			writeError(w, 503, CodeInternalError, "device revocation unavailable")
			return
		}
		writeJSON(w, 200, map[string]bool{"revoked": true})
	})
	router.HandleFunc("POST /auth/renew", func(w http.ResponseWriter, r *http.Request) {
		p, ok := identity.FromContext(r.Context())
		if !ok || !identity.IsRemote(r.Context()) || p.Kind != "device" {
			writeError(w, 403, CodeForbidden, "current device credential required")
			return
		}
		var req DeviceRenewRequest
		if !decodeDeviceBody(w, r, &req) {
			return
		}
		expires, scopes, err := store.RenewDevice(r.Context(), p, identity.CurrentCredentialHash(r.Context()), req.Scopes)
		if err != nil {
			writeError(w, 403, CodeForbidden, "current device renewal refused")
			return
		}
		writeJSON(w, 200, map[string]any{"id": p.ID, "expires_at": expires, "scopes": scopes})
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Pragma", "no-cache")
		if store == nil {
			writeError(w, 503, CodeInternalError, "identity unavailable")
			return
		}
		router.ServeHTTP(w, r)
	})
}

func localOperator(r *http.Request) (identity.Principal, bool) {
	p, ok := identity.FromContext(r.Context())
	return p, ok && !identity.IsRemote(r.Context()) && identity.LocalConnection(r.Context()) && p.ID == identity.OperatorID && p.Kind == "operator"
}

func deviceAdmin(r *http.Request) bool {
	if _, ok := localOperator(r); ok {
		return true
	}
	p, ok := identity.FromContext(r.Context())
	return ok && identity.IsRemote(r.Context()) && identity.HasDeviceScope(p, identity.ScopeAdmin)
}

func decodeDeviceBody(w http.ResponseWriter, r *http.Request, out any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		writeError(w, 400, CodeInvalidRequest, "invalid identity request body")
		return false
	}
	if decoder.Decode(new(any)) != io.EOF {
		writeError(w, 400, CodeInvalidRequest, "invalid identity request body")
		return false
	}
	return true
}
