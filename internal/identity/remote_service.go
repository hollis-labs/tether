package identity

import (
	"context"
	"errors"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Remote service tokens (CW-20261019-0044, PR 3). The remote listener accepts
// stored tth_ device credentials and, on exactly one route, an issued service
// JWT. The route list is closed and has one entry: POST /events.
//
// The two credential families never cross: a bearer is dispatched by format with
// no fallthrough, a device credential never takes the JWT path, and a JWT on
// any other route (or any variant of this one) is refused exactly as before.

// ServiceJWTMethod and ServiceJWTPath are the one route a service JWT may use remotely.
const (
	ServiceJWTMethod = http.MethodPost
	ServiceJWTPath   = "/events"
)

var servicePrincipalRe = regexp.MustCompile(`^msg://service/[a-z][a-z0-9-]{0,31}/[a-z][a-z0-9-]{0,31}$`)

type serviceEventsKey struct{}

// ServiceEventsRequest reports whether this request was admitted by the remote
// service JWT path. Only that path sets it, after full verification.
func ServiceEventsRequest(ctx context.Context) bool {
	ok, _ := ctx.Value(serviceEventsKey{}).(bool)
	return ok
}

// ServiceJWT configures the remote service JWT path. Leaving it out disables the
// path entirely (a JWT is then refused like any other non-device bearer).
type ServiceJWT struct {
	// Verify verifies an issued JWT: signature against the issuer's keys, iss,
	// aud, exp and the shared deny-list (the composite authissuer verifier).
	Verify Verifier
	// Known reports whether jti is a live (issued, unrevoked, unexpired)
	// service token record. A JWT the issuer minted any other way, such as a
	// 15 minute exchange for a service-kind parent, is not accepted here.
	Known func(jti string) bool
	// Failures bounds unauthenticated work: it is charged for every refused JWT.
	Failures *AttemptLimiter
}

// RemoteOption configures RemoteMiddleware.
type RemoteOption func(*remoteConfig)

type remoteConfig struct{ service *ServiceJWT }

// WithServiceJWT enables the remote service JWT path.
func WithServiceJWT(s ServiceJWT) RemoteOption {
	return func(c *remoteConfig) {
		if s.Verify != nil && s.Known != nil {
			c.service = &s
		}
	}
}

// AttemptLimiter is a token bucket charged only for failed attempts.
type AttemptLimiter struct {
	mu     sync.Mutex
	rate   float64
	burst  float64
	tokens float64
	at     time.Time
	now    func() time.Time
}

// NewAttemptLimiter returns a limiter that allows burst failures, refilled at
// rate per second.
func NewAttemptLimiter(rate float64, burst int) *AttemptLimiter {
	return &AttemptLimiter{rate: rate, burst: float64(burst), tokens: float64(burst), now: time.Now}
}

func (l *AttemptLimiter) refillLocked() {
	n := l.now()
	if l.at.IsZero() {
		l.at = n
	}
	l.tokens += n.Sub(l.at).Seconds() * l.rate
	if l.tokens > l.burst {
		l.tokens = l.burst
	}
	l.at = n
}

// Reserve takes one unit of the budget before the work it bounds, atomically,
// and reports whether there was one. Checking and charging in one step is what
// bounds concurrent unauthenticated work: N simultaneous requests cannot all
// pass a "not exhausted" check before any of them is charged.
func (l *AttemptLimiter) Reserve() bool {
	if l == nil {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.refillLocked()
	if l.tokens < 1 {
		return false
	}
	l.tokens--
	return true
}

// Refund returns a unit taken by Reserve when the attempt turned out not to be
// a failure (a verified request, a scope refusal, an unavailable verifier).
func (l *AttemptLimiter) Refund() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.tokens++; l.tokens > l.burst {
		l.tokens = l.burst
	}
}

// serviceJWTRequest reports whether r is exactly POST /events, no query, no
// variant, and returns its JWT bearer if there is one (three dot-separated
// parts, not a tth_ credential).
func serviceJWTRequest(r *http.Request) (string, bool) {
	if r.Method != ServiceJWTMethod || r.URL.Path != ServiceJWTPath || r.URL.EscapedPath() != ServiceJWTPath ||
		r.URL.RawQuery != "" || r.URL.ForceQuery || r.URL.Fragment != "" || r.URL.Opaque != "" {
		return "", false
	}
	headers := r.Header.Values("Authorization")
	if len(headers) != 1 || len(headers[0]) > 4096 {
		return "", false
	}
	parts := strings.Fields(headers[0])
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return "", false
	}
	tok := parts[1]
	if strings.HasPrefix(tok, "tth_") || strings.Count(tok, ".") != 2 {
		return "", false
	}
	return tok, true
}

// handleServiceJWT authenticates a service JWT for POST /events. It writes the
// refusal itself, or calls next with the verified principal.
func (c *ServiceJWT) handle(w http.ResponseWriter, r *http.Request, token string, record func(context.Context, Observation) error, next http.Handler) {
	obs := Observation{At: time.Now().UTC(), Mode: Enforce, Method: r.Method, Route: ServiceJWTPath, Authentication: "invalid"}
	// One unit is reserved before the expensive verification and kept only if the
	// attempt is a refused credential (401). Everything else gives it back, on
	// every path including a panic, so a success, a 403 scope refusal, a 503 and
	// a cancelled verification never cost the budget.
	if !c.Failures.Reserve() {
		w.Header().Set("Retry-After", "1")
		authError(w, http.StatusTooManyRequests, "rate_limited")
		return
	}
	charged, settled := false, false
	settle := func() {
		if !charged && !settled {
			settled = true
			c.Failures.Refund()
		}
	}
	defer settle()
	refuse := func(status int, code string) {
		charged = status == http.StatusUnauthorized
		if record != nil {
			_ = record(r.Context(), obs)
		}
		authError(w, status, code)
	}
	p, err := c.Verify.Verify(r.Context(), token)
	switch {
	case errors.Is(err, ErrInvalidToken):
		refuse(http.StatusUnauthorized, "unauthorized")
		return
	case err != nil:
		obs.Authentication = "unavailable"
		w.Header().Set("Retry-After", "5")
		if record != nil {
			_ = record(r.Context(), obs)
		}
		authError(w, http.StatusServiceUnavailable, "identity_unavailable")
		return
	}
	// Everything below must hold; any miss is the same 401 as a bad signature.
	if p.Kind != "service" || p.TokenID == "" || p.Audience != "tether" || !servicePrincipalRe.MatchString(p.ID) ||
		p.ExpiresAt == nil || !c.Known(p.TokenID) {
		refuse(http.StatusUnauthorized, "unauthorized")
		return
	}
	obs.Authentication, obs.Verified, obs.PrincipalID = "verified", true, p.ID
	if !ScopeGranted(p.Scopes, ScopeEvents) {
		obs.Event = "service_jwt_scope_required"
		if record != nil {
			_ = record(r.Context(), obs)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":"scope_required","message":"required scope: tether.events"}}`))
		return
	}
	p.Verified = true
	// Audit-style line: principal and a jti prefix only, never the token.
	log.Printf("identity: remote service JWT accepted sub=%s jti=%.8s route=POST %s", p.ID, p.TokenID, ServiceJWTPath)
	if record != nil {
		obs.Event = "service_jwt_accepted"
		_ = record(r.Context(), obs)
	}
	// Give the unit back before the handler runs: a slow publish must not hold
	// a failure-budget unit for its whole duration.
	settle()
	ctx := context.WithValue(WithPrincipal(r.Context(), p), serviceEventsKey{}, true)
	next.ServeHTTP(w, r.WithContext(ctx))
}
