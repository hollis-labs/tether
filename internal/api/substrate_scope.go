package api

import (
	"net/http"
	"strings"

	messaging "github.com/hollis-labs/libs/mesh/core/messaging"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/registry"
)

// Substrate-limited grants (CW-20261024-0023, CW-20261025-0001).
//
// A service credential can hold "tether.registry:nanite": it may register, update and
// deregister only actors that carry an external id of substrate "nanite", and read the
// registry. It can hold "tether.send_as:nanite": it may author messages as such actors.
// Actors share the msg://agent/agent-mux/ URN namespace whatever their substrate, so the
// registry's external ids, not the URN, say who is a nanite actor.
//
// Like every identity finding here, a violation is recorded in observe mode and refused
// in enforce mode; the qualified scope is what lets the credential through the route
// table at all.
const (
	eventSubstrateUnverified = "registry_substrate_unverified"
	eventSubstrateRefused    = "registry_substrate_refused"
)

// qualifiedRegistryRoute reports whether r is a registry route a substrate-limited
// credential may reach: reads, register (POST /registry/{kind}), and update or
// deregister of one entry. Sync, merge, bootstrap, reonboard and scoped bindings stay
// with the unrestricted scope.
func qualifiedRegistryRoute(r *http.Request) bool {
	rest, ok := strings.CutPrefix(r.URL.EscapedPath(), "/registry/")
	if !ok || rest == "" {
		return false
	}
	switch parts := strings.Split(rest, "/"); {
	case len(parts) == 1:
		return r.Method == http.MethodGet || r.Method == http.MethodPost
	case len(parts) == 2:
		return r.Method == http.MethodGet || r.Method == http.MethodPatch || r.Method == http.MethodDelete
	}
	return false
}

// holdsQualifiedRegistry reports whether the request's principal holds a
// tether.registry:<substrate> grant and the route is one such a grant covers.
func holdsQualifiedRegistry(r *http.Request) bool {
	p, ok := identity.FromContext(r.Context())
	return ok && qualifiedRegistryRoute(r) && len(identity.Qualifiers(p.Scopes, identity.AppScopePrefix+identity.ScopeRegistry)) > 0
}

// registrySubstrateLimit returns the substrates the caller's registry writes are limited
// to. limited is false for an unrestricted caller (plain tether.registry, the operator's
// "*") and for a caller without any qualified grant, whose access the route gate decides.
func registrySubstrateLimit(r *http.Request) (allowed []string, limited bool) {
	p, ok := verifiedPrincipal(r)
	if !ok || identity.ScopeGranted(p.Scopes, identity.ScopeRegistry) {
		return nil, false
	}
	allowed = identity.Qualifiers(p.Scopes, identity.AppScopePrefix+identity.ScopeRegistry)
	return allowed, len(allowed) > 0
}

func hasSubstrate(exts []registry.ExternalID, allowed []string) bool {
	for _, e := range exts {
		for _, a := range allowed {
			if e.Substrate == a {
				return true
			}
		}
	}
	return false
}

// refuseOutsideSubstrate applies the finding for a substrate-limited write: recorded in
// observe mode (returns true, the handler continues), a 403 in enforce mode.
func (s *Server) refuseOutsideSubstrate(w http.ResponseWriter, r *http.Request, allowed []string, what string) bool {
	p, _ := verifiedPrincipal(r)
	if s.enforcing() {
		s.auditIdentity(r, p, eventSubstrateRefused)
		writeError(w, http.StatusForbidden, CodeForbidden, what+" is outside the substrate this credential is limited to ("+strings.Join(allowed, ", ")+")")
		return false
	}
	s.auditIdentity(r, p, eventSubstrateUnverified)
	return true
}

// checkRegistryEntry limits an update or deregister of one entry to the credential's
// substrates. It reports whether the handler may continue.
func (s *Server) checkRegistryEntry(w http.ResponseWriter, r *http.Request, urn string) bool {
	allowed, limited := registrySubstrateLimit(r)
	if !limited {
		return true
	}
	exts, err := s.Registry.LookupExternalIDsForURN(r.Context(), urn)
	if err != nil {
		writeRegistryError(w, err)
		return false
	}
	if hasSubstrate(exts, allowed) {
		return true
	}
	return s.refuseOutsideSubstrate(w, r, allowed, "this actor")
}

// mayActAsViaSubstrate reports whether p holds tether.send_as:<substrate> for a substrate
// the actor at addr carries an external id of.
func (s *Server) mayActAsViaSubstrate(r *http.Request, p identity.Principal, addr string) bool {
	allowed := identity.Qualifiers(p.Scopes, identity.ScopeSendAsSubstrate)
	if len(allowed) == 0 || s.Registry == nil {
		return false
	}
	if _, err := messaging.ParseURN(addr); err != nil {
		return false
	}
	exts, err := s.Registry.LookupExternalIDsForURN(r.Context(), addr)
	return err == nil && hasSubstrate(exts, allowed)
}
