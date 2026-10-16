package api

import (
	"net/http"
	"strings"
	"time"

	messaging "github.com/hollis-labs/libs/mesh/core/messaging"
	"github.com/hollis-labs/tether/internal/identity"
)

// Envelope metadata the daemon writes for a verified principal's message.
// Callers cannot set them: they are removed from the request first.
const (
	// MetaFromVerification is "stamped" (from was absent and the daemon filled
	// it from the principal), "verified" (the supplied from is in the
	// principal's address set or covered by message.send_as) or "unverified"
	// (observe mode kept a from the principal may not author).
	MetaFromVerification = "from_verification"
	// MetaFromUnverified is "true" exactly when MetaFromVerification is
	// "unverified", so a receiver can test one key.
	MetaFromUnverified = "from_unverified"
	// MetaFromPrincipal is the verified principal that sent the message.
	MetaFromPrincipal = "from_principal"
)

const (
	eventFromUnverified = "from_unverified"
	eventFromRefused    = "from_refused"
	eventAsUnverified   = "as_unverified"
	eventAsRefused      = "as_refused"
)

// enforcing reports whether sender and ?as= findings are refused (403) rather
// than recorded. Only an explicit enforce mode refuses.
func (s *Server) enforcing() bool { return s.IdentityMode == identity.Enforce }

func (s *Server) auditIdentity(r *http.Request, p identity.Principal, event string) {
	if s.IdentityAudit == nil {
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/")
	family, _, _ := strings.Cut(path, "/")
	_ = s.IdentityAudit(r.Context(), identity.Observation{
		At: time.Now().UTC(), PrincipalID: p.ID, SessionID: p.SessionID,
		Mode: s.IdentityMode, Authentication: "verified",
		Method: r.Method, Route: "/" + family, Event: event,
	})
}

func canonicalAddress(a messaging.Address) string { return a.URN() }

// stampSender applies the server-stamped sender rule to env for the request's
// verified principal. It returns false after writing a 403 (enforce only).
// Without a verified principal nothing changes: the middleware, not this
// function, decides whether anonymous traffic is admitted.
func (s *Server) stampSender(w http.ResponseWriter, r *http.Request, env *messaging.Envelope) bool {
	delete(env.Metadata, MetaFromVerification)
	delete(env.Metadata, MetaFromUnverified)
	delete(env.Metadata, MetaFromPrincipal)
	p, ok := verifiedPrincipal(r)
	if !ok {
		return true
	}
	mark := func(state string) {
		if env.Metadata == nil {
			env.Metadata = map[string]string{}
		}
		env.Metadata[MetaFromVerification] = state
		env.Metadata[MetaFromPrincipal] = p.ID
		if state == "unverified" {
			env.Metadata[MetaFromUnverified] = "true"
		}
	}
	if env.From.IsZero() {
		addr, err := messaging.ParseURN(p.DefaultAddress())
		if err != nil {
			return true // no stampable address; the caller's from-required check answers
		}
		env.From = addr
		mark("stamped")
		return true
	}
	if p.MayActAs(canonicalAddress(env.From)) {
		mark("verified")
		return true
	}
	if s.enforcing() {
		s.auditIdentity(r, p, eventFromRefused)
		writeError(w, http.StatusForbidden, CodeForbidden, "from is not an address of the authenticated principal")
		return false
	}
	mark("unverified")
	s.auditIdentity(r, p, eventFromUnverified)
	return true
}

// verifiedPrincipal is the request's principal when the middleware verified
// its credential. A claim-only or internally built principal never stamps or
// vouches for an address.
func verifiedPrincipal(r *http.Request) (identity.Principal, bool) {
	p, ok := identity.FromContext(r.Context())
	return p, ok && p.Verified
}

func canonicalClaim(claimed string) string {
	if addr, err := messaging.ParseURN(claimed); err == nil {
		return canonicalAddress(addr)
	}
	return claimed
}

// verifyClaim checks an address the caller claims (a ?as=, a body by/from/
// member, a broker sender) against the verified principal's address set.
// Observe records unverifiedEvent and continues; enforce records refusedEvent
// and answers 403. With no verified principal the claim is left to the
// middleware. It reports whether the handler may continue.
func (s *Server) verifyClaim(w http.ResponseWriter, r *http.Request, claimed, unverifiedEvent, refusedEvent, what string) bool {
	p, ok := verifiedPrincipal(r)
	if !ok || claimed == "" || p.MayActAs(canonicalClaim(claimed)) {
		return true
	}
	if s.enforcing() {
		s.auditIdentity(r, p, refusedEvent)
		writeError(w, http.StatusForbidden, CodeForbidden, what+" is not an address of the authenticated principal")
		return false
	}
	s.auditIdentity(r, p, unverifiedEvent)
	return true
}

// claimedActor is verifyClaim for a body or query field that names the acting
// member (by, member, as, from).
func (s *Server) claimedActor(w http.ResponseWriter, r *http.Request, claimed, field string) bool {
	return s.verifyClaim(w, r, claimed, eventAsUnverified, eventAsRefused, field)
}

// stampedActor returns the claimed address, or the verified principal's own
// address when none was given. Without a verified principal it returns claimed
// unchanged.
func stampedActor(r *http.Request, claimed string) string {
	if claimed != "" {
		return claimed
	}
	if p, ok := verifiedPrincipal(r); ok {
		return p.DefaultAddress()
	}
	return ""
}

// guardAs verifies a ?as= mailbox identity against the verified principal's
// address set (or message.send_as) before the wrapped handler runs. Observe
// records as_unverified and continues; enforce refuses with 403.
func (s *Server) guardAs(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.claimedActor(w, r, r.URL.Query().Get("as"), "as") {
			return
		}
		next(w, r)
	}
}

// senderMetadata returns the daemon-stamped sender keys of m, for carrying onto
// a stored reply. Nil when the request had no verified principal.
func senderMetadata(m map[string]string) map[string]string {
	var out map[string]string
	for _, k := range []string{MetaFromVerification, MetaFromUnverified, MetaFromPrincipal} {
		if v, ok := m[k]; ok {
			if out == nil {
				out = map[string]string{}
			}
			out[k] = v
		}
	}
	return out
}
