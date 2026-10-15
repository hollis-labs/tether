package identity

import (
	"context"
	"errors"
	"io"
	"log"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	auth "github.com/hollis-labs/libs/security/auth"
)

// requestSlot carries per-request data between the go-auth verifier adapter,
// the audit hook and the handler wrapped by the guard. go-auth's Principal
// has no room for Tether's address set, so the adapter parks the full
// identity.Principal here.
type requestSlot struct {
	mu           sync.Mutex
	principal    *Principal
	scoped       ScopeObservation
	recordFailed bool
}

type slotKey struct{}

func slotFrom(ctx context.Context) *requestSlot {
	s, _ := ctx.Value(slotKey{}).(*requestSlot)
	return s
}

// authVerifier adapts an identity.Verifier to go-auth's Verifier.
//
// Scopes: an issued JWT (TokenID set) carries its own scopes, enforced by
// api.AgentScopeMiddleware. A stored tth_ credential keeps today's local
// semantics, where any verified principal reaches the local API and scope
// policy is observation only (CW-20261010-0261); it is presented to the guard
// with "*" so go-auth does not narrow it.
type authVerifier struct{ v Verifier }

func (a authVerifier) Verify(ctx context.Context, token string) (auth.Principal, error) {
	if a.v == nil {
		return auth.Principal{}, auth.ErrUnavailable
	}
	p, err := a.v.Verify(ctx, token)
	switch {
	case err == nil:
	case errors.Is(err, ErrInvalidToken):
		return auth.Principal{}, auth.ErrInvalidToken
	default:
		return auth.Principal{}, auth.ErrUnavailable
	}
	p.Verified = true
	if slot := slotFrom(ctx); slot != nil {
		slot.mu.Lock()
		cp := p
		slot.principal = &cp
		slot.mu.Unlock()
	}
	out := auth.Principal{Verified: true, ID: p.ID, Kind: p.Kind, Display: p.Display, Audience: p.Audience,
		SessionID: p.SessionID, Parent: p.Parent, TokenID: p.TokenID}
	if p.ExpiresAt != nil {
		out.ExpiresAt = *p.ExpiresAt
	}
	if p.TokenID != "" {
		out.Scopes = append([]string(nil), p.Scopes...)
	} else {
		out.Scopes = []string{"*"}
	}
	return out, nil
}

var quietLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

func authMode(m Mode) auth.Mode {
	switch m.Canonical() {
	case Enforce:
		return auth.ModeEnforce
	case Observe:
		return auth.ModePermissive
	case Off, Permissive: // Permissive is canonicalized above; Off is the fallthrough
	}
	return auth.ModeOff
}

// localMiddleware is the local listener's identity middleware: go-auth's
// Guard supplies the mode logic (off/permissive/enforce), bearer parsing and
// verification; Tether keeps its audit rows, scope attribution and
// route exemptions.
func localMiddleware(mode Mode, verifier Verifier, record func(context.Context, Observation) error, scopes ScopePolicy, next http.Handler) http.Handler {
	if err := mode.Validate(); err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			authError(w, http.StatusServiceUnavailable, "identity_unavailable")
		})
	}
	mode = mode.Canonical()
	if mode == "" {
		mode = Off
	}
	offAudit := &offAuditLimiter{}
	guard, err := auth.New(auth.Config{
		Mode:     authMode(mode),
		Verifier: authVerifier{verifier},
		Logger:   quietLogger, // would-deny is recorded in identity_audit, not logged per request
		OpenPath: func(p string) bool {
			return p == "/health" || p == JWKSPath || p == "/a2a" || strings.HasPrefix(p, "/a2a/")
		},
		OnRequest: func(r *http.Request, p auth.Principal, ev auth.Event) {
			slot := slotFrom(r.Context())
			if ev.Authentication == auth.StateOffMode {
				offAudit.note(ev)
				return
			}
			if mode == Off || slot == nil || ev.Authentication == auth.StateMissing {
				return // anonymous traffic never allocates audit metadata or touches the database
			}
			o := Observation{At: ev.At, Mode: mode, Authentication: ev.Authentication, Verified: ev.Verified, Method: r.Method, Route: routeFamily(r.URL.Path)}
			slot.mu.Lock()
			ip := slot.principal
			slot.mu.Unlock()
			if ev.Authentication == auth.StateVerified && ip != nil {
				o.PrincipalID, o.SessionID = ip.ID, ip.SessionID
				if scopes.Observe != nil {
					sp := *ip
					slot.scoped = scopes.Observe(r.WithContext(WithPrincipal(r.Context(), sp)), sp)
					if slot.scoped.Known {
						allowed := slot.scoped.WouldAllow
						o.RoutePattern, o.RequiredScope, o.WouldAllow = slot.scoped.Pattern, slot.scoped.Required, &allowed
					}
				}
			}
			// Context lookups and verified proxy telemetry are self-observation:
			// authenticate normally but do not duplicate each call in the audit.
			if o.Verified && (r.Method == http.MethodGet && r.URL.Path == "/auth/context" || r.Method == http.MethodPost && r.URL.Path == "/proxy/events") {
				return
			}
			if record != nil {
				if err := record(r.Context(), o); err != nil {
					// Do not print database errors: a driver may echo parameters.
					log.Printf("identity: observation could not be persisted")
					slot.recordFailed = true
				}
			}
		},
	})
	if err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			authError(w, http.StatusServiceUnavailable, "identity_unavailable")
		})
	}
	log.Printf("identity: %s", guard.Status())
	inner := guard.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slot := slotFrom(r.Context())
		if mode == Enforce && slot.recordFailed {
			authError(w, http.StatusServiceUnavailable, "identity_unavailable")
			return
		}
		if scopes.Enforce && slot.scoped.Known && !slot.scoped.WouldAllow {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"code":"scope_required","message":"required scope: ` + slot.scoped.Required + `"}}`))
			return
		}
		slot.mu.Lock()
		p := slot.principal
		slot.mu.Unlock()
		if p != nil {
			r = r.WithContext(WithPrincipal(r.Context(), *p))
		}
		next.ServeHTTP(w, r)
	}))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(context.WithValue(r.Context(), slotKey{}, &requestSlot{}))
		inner.ServeHTTP(w, r)
	})
}

// offAuditLimiter makes the off switch visible without a line per request:
// the first anonymous request and then a summary at most every five minutes.
type offAuditLimiter struct {
	mu    sync.Mutex
	last  time.Time
	count int
}

func (l *offAuditLimiter) note(ev auth.Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.count++
	if !l.last.IsZero() && ev.At.Sub(l.last) < 5*time.Minute {
		return
	}
	log.Printf("identity: auth=off; %d request(s) attributed to %s (most recent %s %s)", l.count, auth.OffModePrincipalID, ev.Method, ev.Route)
	l.last, l.count = ev.At, 0
}
