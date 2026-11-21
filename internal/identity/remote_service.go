package identity

import (
	"context"
	"errors"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// Remote service tokens (CW-20261019-0044, PR 3). The remote listener accepts
// stored tth_ device credentials and, on a closed list of exact routes, an
// issued service JWT on the send routes below, the bound mailbox routes
// (CW-20261118-0017, CW-20261121-0142), and read-only registry agent lookups
// (CW-20261121-0142). Mailbox grants are checked independently of sender mode.
//
// The two credential families never cross: a bearer is dispatched by format with
// no fallthrough, a device credential never takes the JWT path, and a JWT on
// any other route (or any variant of this one) is refused exactly as before.

// ServiceJWTMethod and ServiceJWTPath are the events route a service JWT may use remotely.
const (
	ServiceJWTMethod = http.MethodPost
	ServiceJWTPath   = "/events"
)

// serviceJWTRoutes lists the exact send routes a service JWT may use
// remotely, with the scope each requires. A scope for one route never opens
// another: tether.messages does not publish events and tether.events does not
// send mail.
var serviceJWTRoutes = map[string]string{
	"/events":          ScopeEvents,
	"/messages":        ScopeMessages,
	"/messages/notify": ScopeMessages,
	"/messages/post":   ScopeMessages,
}

// serviceRouteName returns the listed route equal to escapedPath (a fixed route
// template, never the request's own string), or "-" for a path off the list.
func serviceRouteName(escapedPath string) string {
	for route := range serviceJWTRoutes {
		if route == escapedPath {
			return route
		}
	}
	if escapedPath == "/messages/list" {
		return "/messages/list"
	}
	if serviceReadPath.MatchString(escapedPath) {
		return "/messages/{id}/read"
	}
	if isServiceMessageGetPath(escapedPath) {
		return "/messages/{id}"
	}
	if isServiceRegistryAgentPath(escapedPath) {
		return "/registry/agents/{urn}"
	}
	return "-"
}

// ServiceJWTScope returns the scope a service JWT needs for method and escaped
// path, and whether the route is on the closed list at all.
func ServiceJWTScope(method, escapedPath string) (string, bool) {
	if method == http.MethodGet {
		switch {
		case escapedPath == "/messages/list", isServiceMessageGetPath(escapedPath):
			return ScopeRead, true
		case isServiceRegistryAgentPath(escapedPath):
			return ScopeRegistryRead, true
		}
		return "", false
	}
	if method == http.MethodPost && serviceReadPath.MatchString(escapedPath) {
		return ScopeMessages, true
	}
	if method != ServiceJWTMethod {
		return "", false
	}
	scope, ok := serviceJWTRoutes[escapedPath]
	return scope, ok
}

var (
	serviceReadPath       = regexp.MustCompile(`^/messages/[A-Za-z0-9_-]+/read$`)
	serviceMessageGetPath = regexp.MustCompile(`^/messages/[A-Za-z0-9_-]+$`)
	// serviceRegistryURNRe matches a canonical msg:// URN (2 or 3 path segments
	// after msg://, e.g. msg://agent/agent-mux/agt_xxx or msg://user/chrispian)
	// with no dot-segments, whitespace or control characters.
	serviceRegistryURNRe = regexp.MustCompile(`^msg://[a-z][a-z0-9-]*(?:/[A-Za-z0-9][A-Za-z0-9._#:-]*){1,2}$`)
)

// reservedMessageSegments are static routes under /messages/ that must never
// be admitted as GET /messages/{id}.
var reservedMessageSegments = map[string]bool{
	"list":            true,
	"inbox":           true,
	"watch":           true,
	"subscribe":       true,
	"notify":          true,
	"request":         true,
	"post":            true,
	"aliases":         true,
	"pending-replies": true,
	"thread":          true,
	"retention":       true,
}

func isServiceMessageGetPath(escapedPath string) bool {
	if !serviceMessageGetPath.MatchString(escapedPath) {
		return false
	}
	return !reservedMessageSegments[strings.TrimPrefix(escapedPath, "/messages/")]
}

// isServiceRegistryAgentPath reports whether escapedPath is an exact,
// single-segment GET /registry/agents/{escaped-urn} path. The /registry/agents/
// prefix must be literal, the trailing segment must contain no unescaped slash,
// and it must be a canonical percent-encoding of a msg:// URN.
func isServiceRegistryAgentPath(escapedPath string) bool {
	seg, ok := strings.CutPrefix(escapedPath, "/registry/agents/")
	if !ok || seg == "" || strings.Contains(seg, "/") {
		return false
	}
	urn, err := url.PathUnescape(seg)
	if err != nil || !serviceRegistryURNRe.MatchString(urn) {
		return false
	}
	canon := url.PathEscape(urn)
	return strings.EqualFold(seg, canon) ||
		strings.EqualFold(seg, strings.ReplaceAll(canon, ":", "%3A")) ||
		strings.EqualFold(seg, strings.ReplaceAll(canon, "%3A", ":"))
}

var servicePrincipalRe = regexp.MustCompile(`^msg://service/[a-z][a-z0-9-]{0,31}/[a-z][a-z0-9-]{0,31}$`)

type serviceEventsKey struct{}

// ServiceEventsRequest reports whether this request was admitted by the remote
// service JWT path (on any route of the closed list). Only that path sets it,
// after full verification. The route's scope is judged by ServiceJWTScope.
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

// serviceJWTRequest reports whether r is exactly one of the closed routes,
// with only its documented mailbox query selectors, no variant, and
// returns its JWT bearer if there is one (three dot-separated parts, not a tth_
// credential).
func serviceJWTRequest(r *http.Request) (string, bool) {
	escaped := r.URL.EscapedPath()
	if _, listed := ServiceJWTScope(r.Method, escaped); !listed ||
		(!isServiceRegistryAgentPath(escaped) && escaped != r.URL.Path) ||
		!serviceJWTQuery(r) || r.URL.ForceQuery || r.URL.Fragment != "" || r.URL.Opaque != "" {
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

// serviceMailboxRequest is deliberately narrower than all mailbox operations.
func serviceMailboxRequest(r *http.Request) bool {
	escaped := r.URL.EscapedPath()
	return r.Method == http.MethodGet && (escaped == "/messages/list" || isServiceMessageGetPath(escaped)) ||
		r.Method == http.MethodPost && serviceReadPath.MatchString(escaped)
}

// Queries are allowed only on the bound mailbox routes, with one unambiguous
// value per documented selector. No unknown keys or duplicate selectors.
func serviceJWTQuery(r *http.Request) bool {
	if !serviceMailboxRequest(r) {
		return r.URL.RawQuery == ""
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return false
	}
	isList := r.Method == http.MethodGet && r.URL.EscapedPath() == "/messages/list"
	allowed := map[string]bool{"as": true}
	if isList {
		for _, key := range []string{"to", "kind", "thread_id", "include_archived", "unread_only", "limit", "offset"} {
			allowed[key] = true
		}
	}
	for _, pair := range strings.Split(r.URL.RawQuery, "&") {
		key, _, hasValue := strings.Cut(pair, "=")
		if !hasValue || !allowed[key] || len(q[key]) != 1 {
			return false
		}
	}
	return q.Get("as") != "" && (!isList || q.Get("to") == q.Get("as"))
}

// handle authenticates a service JWT for the closed route list. It writes the
// refusal itself, or calls next with the verified principal.
func (c *ServiceJWT) handle(w http.ResponseWriter, r *http.Request, token string, record func(context.Context, Observation) error, next http.Handler) {
	escaped := r.URL.EscapedPath()
	need, _ := ServiceJWTScope(r.Method, escaped)
	obs := Observation{At: time.Now().UTC(), Mode: Enforce, Method: r.Method, Route: r.URL.Path, Authentication: "invalid"}
	// One unit is reserved before the expensive verification and kept only if the
	// attempt is a refused credential (401). Everything else gives it back, on
	// every path including a panic, so a success, a 403 scope refusal, a 503 and
	// a canceled verification never cost the budget.
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
	if !ScopeGranted(p.Scopes, need) {
		obs.Event = "service_jwt_scope_required"
		if record != nil {
			_ = record(r.Context(), obs)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":"scope_required","message":"required scope: ` + AppScopePrefix + need + `"}}`))
		return
	}
	if serviceMailboxRequest(r) {
		// A service's own subject and aliases are not agent mailbox grants.
		// This is mandatory even when sender identity is in observe mode.
		as := r.URL.Query().Get("as")
		if !strings.HasPrefix(as, "msg://agent/") || !slices.Contains(p.Addresses, as) {
			refuse(http.StatusForbidden, "forbidden")
			return
		}
	}
	p.Verified = true
	// Audit-style line: principal and a jti prefix only, never the token.
	method := http.MethodPost
	if r.Method == http.MethodGet {
		method = http.MethodGet
	}
	log.Printf("identity: remote service JWT accepted sub=%s jti=%.8s route=%s %s scope=%s", p.ID, p.TokenID, method, serviceRouteName(escaped), need)
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
