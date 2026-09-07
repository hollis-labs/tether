package api

// whoami.go — T08 (messaging vNext, CW-20260906-0039): the self-discovery
// surface the architecture doc calls for explicitly: "Expose
// self-discovery returning the caller's actual address, identity
// mappings, memberships, host, and negotiated capabilities." Before this
// file, nothing in the codebase implemented any form of self-discovery
// at all (confirmed by a repo-wide search during T08 design research).
//
// Same-host, self-asserted trust model (ADR 0045): ?as= is the caller's
// own claimed identity -- there is nothing to authorize here beyond
// requiring the caller to name who they're asking about, matching every
// other same-host read on this surface. Every sub-lookup (profile,
// external ids, memberships, current binding) is independently
// best-effort: an unregistered or never-bound caller (a private-local,
// standalone session that never published) still gets a 200 with the
// fields that apply to it, since self-discovery must work "without
// requiring online Tether" registration -- ADR 0045's trust model is
// about identity assertion, not a precondition that the identity be
// already known to the registry.
//
// Profile/Groups redaction (added during T11's independent security
// review, CW-20260906-0042): ?as= is never verified against the real
// caller, so whoami is reachable for ANY urn a same-host caller names,
// not only "the caller's own." Before this fix, out.Profile embedded the
// full, unredacted registry.Profile -- including Callback, HostAddress,
// KindMeta and ExternalIDs, the exact four fields T09's redactProfile
// (registry.go) strips from Search/Lookup specifically because they're
// "public discovery" reachable by any same-host caller. whoami is that
// same shape of caller (no stronger identity check than Search/Lookup
// has), so it was a complete, un-audited bypass of T09's redaction:
// anyone could recover via whoami exactly what Search/Lookup had just
// hidden. out.Profile and each entry in out.Groups (also a
// registry.Profile, kind=group, with the same four sensitive fields) are
// now redacted the identical way.
//
// out.ExternalIDs and out.Binding are deliberately NOT redacted here,
// unlike the embedded Profile.ExternalIDs this redaction removes: they
// are T08's own explicit, distinct self-discovery mandate ("identity
// mappings... host, and negotiated capabilities" -- ARCHITECTURE.md),
// carrying a different, narrower disclosure than Callback/HostAddress/
// KindMeta (an arbitrary per-kind operational JSON blob and an operator
// config pointer, respectively). Narrowing those two further would
// retroactively defeat T08's own shipped, reviewed acceptance criteria
// for a self-discovery endpoint whose entire purpose is exposing exactly
// this; the reviewer's own fix-shape guidance treated them as a separate
// question from Profile's four fields.

import (
	"errors"
	"net/http"

	"github.com/hollis-labs/tether/internal/registry"
)

// registerWhoamiRoutes mounts GET /whoami. Only attached when Server.Registry
// is non-nil, matching the registry/bindings routes' nil-disables-route
// convention.
func (s *Server) registerWhoamiRoutes(mux *http.ServeMux) {
	if s.Registry == nil {
		return
	}
	mux.HandleFunc("/whoami", s.handleWhoami)
}

// whoamiResponse is the self-discovery payload. Fields are individually
// nil/empty when that lookup found nothing for the caller, never because
// of an error the caller can't see -- see handleWhoami's per-lookup
// error handling. Profile and Groups are redacted (see file doc comment);
// ExternalIDs and Binding are not.
type whoamiResponse struct {
	URN         string                   `json:"urn"`
	Profile     *redactedProfile         `json:"profile,omitempty"`
	ExternalIDs []registry.ExternalID    `json:"external_ids,omitempty"`
	Groups      []redactedProfile        `json:"groups,omitempty"`
	Binding     *registry.RuntimeBinding `json:"binding,omitempty"`
}

func (s *Server) handleWhoami(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
		return
	}
	as := r.URL.Query().Get("as")
	if as == "" {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "as is required")
		return
	}

	out := whoamiResponse{URN: as}
	ctx := r.Context()

	if profile, err := s.Registry.Lookup(ctx, as); err == nil {
		redacted := redactProfile(profile)
		out.Profile = &redacted
	} else if !errors.Is(err, registry.ErrNotFound) {
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}

	// Unlike Profile/Binding, LookupExternalIDsForURN has no "not found"
	// sentinel at all (storage.go's implementation is a plain query that
	// returns an empty slice for "no rows," not an error) -- so ANY error
	// here is a real, unexpected failure and must be re-raised, not
	// silently swallowed into "no external ids" the way a distinct
	// review pass found this originally doing.
	ids, err := s.Registry.LookupExternalIDsForURN(ctx, as)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}
	if len(ids) > 0 {
		out.ExternalIDs = ids
	}

	if s.Groups != nil {
		if groups, err := s.Groups.ListGroupsForMember(ctx, as); err == nil && len(groups) > 0 {
			out.Groups = redactProfiles(groups)
		}
	}

	if binding, err := s.Registry.CurrentBinding(ctx, as); err == nil {
		out.Binding = &binding
	} else if !errors.Is(err, registry.ErrBindingNotFound) {
		// registry.ErrBindingNotFound is a DISTINCT sentinel from
		// registry.ErrNotFound (bindings.go) -- CurrentBinding never
		// returns the latter, so checking for it here would incorrectly
		// 500 on the common "never bound" case.
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, out)
}
