package api

import (
	"net/http"
	"slices"

	"github.com/hollis-labs/tether/internal/identity"
)

// LocalRouteScopes is the local socket's route-to-scope table (CW-20261010-0261).
// It is the same table type, matcher and scope vocabulary as RemoteRouteScopes
// and starts as a copy of it, so a route registered once has one required
// scope on both listeners. A local divergence is a deliberate edit here, not
// a second hand-maintained list. It is observe-only: the daemon records the
// verdict in identity_audit and never rejects on it.
var LocalRouteScopes = slices.Clone(RemoteRouteScopes)

// RequiredLocalScope returns the matched table pattern and required scope.
// known is false for an unlisted path or method.
func RequiredLocalScope(r *http.Request) (pattern, scope string, known bool) {
	rule := scopeRule(LocalRouteScopes, r)
	if rule == nil {
		return "", "", false
	}
	scope, known = rule.Methods[r.Method]
	return rule.Path, scope, known
}

// WouldAllowLocal reports whether the principal's granted scopes satisfy a
// table scope. The local operator holds "*". Other principals must hold the
// scope literally; session principals carry legacy session.write-style
// scopes, so they report false until a mapping is decided (observe data
// exists to inform that). "local-only" means operator custody.
func WouldAllowLocal(p identity.Principal, scope string) bool {
	switch scope {
	case publicScope:
		return true
	case selfScope:
		return p.Kind == "device" || slices.Contains(p.Scopes, "*")
	}
	if p.TokenID != "" { // issued token: namespaced scopes, no local-custody authority
		return scope != localScope && identity.ScopeGranted(p.Scopes, scope)
	}
	return slices.Contains(p.Scopes, "*") || scope != localScope && identity.ScopeGranted(p.Scopes, scope)
}

// ObserveLocalScope evaluates a verified principal's request against
// LocalRouteScopes.
func ObserveLocalScope(r *http.Request, p identity.Principal) identity.ScopeObservation {
	pattern, scope, known := RequiredLocalScope(r)
	if !known {
		return identity.ScopeObservation{}
	}
	return identity.ScopeObservation{Pattern: pattern, Required: scope, WouldAllow: WouldAllowLocal(p, scope), Known: true}
}
