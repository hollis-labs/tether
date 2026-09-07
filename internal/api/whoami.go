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
// error handling.
type whoamiResponse struct {
	URN         string                   `json:"urn"`
	Profile     *registry.Profile        `json:"profile,omitempty"`
	ExternalIDs []registry.ExternalID    `json:"external_ids,omitempty"`
	Groups      []registry.Profile       `json:"groups,omitempty"`
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
		out.Profile = &profile
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
			out.Groups = groups
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
