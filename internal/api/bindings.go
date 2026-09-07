package api

// bindings.go — T07 (messaging vNext, CW-20260906-0038): the published-
// local bridge registration surface. A "published-local" actor is one
// Tether does not launch or manage a session for at all -- an external
// process (a bridge) leases a RuntimeBinding (the T02 primitive, already
// used internally by T06's local-session launch path) over HTTP instead
// of through the Go registry.Service call session_lifecycle.go uses.
//
// Scope, deliberately narrow (architecture: "Tether never assumes launch/
// resume authority over" a published-local actor; "no production peer
// setup implied" for this task):
//   - This endpoint ALWAYS mints VisibilityPublishedLocal bindings. A
//     caller cannot self-declare VisibilityPrivateLocal or
//     VisibilityTetherHosted -- those are Tether's own internal
//     designations for sessions it actually launched (session_lifecycle.go
//     leaseActorBinding), never a claim an external caller can make over
//     this public surface.
//   - Capabilities must be exactly ["pull-only"]. Push-notified bridging
//     (Tether calling an external webhook to deliver a wake) is NOT
//     implemented -- a caller asking for anything else is told so
//     honestly (400) rather than silently accepted and later failing to
//     ever wake. "pull-only" means the bridge fetches its own mailbox via
//     the existing GET /messages/inbox|list (already caller-scoped by
//     ?as=, ADR 0045) and durably claims/acks/nacks via the endpoints in
//     this file on its own schedule -- Tether never initiates contact.
//   - Same-host, self-asserted trust model throughout (ADR 0045): a
//     binding_id is an opaque, unguessable UUID (registry/bindings.go),
//     and knowledge of it is what authorizes renew/revoke/claim/ack/nack
//     against it -- the same bearer-style convention already used for
//     message/delivery ids (?as= on /messages/*). No new authentication
//     primitive is introduced here.
//
// ResolveActorSession (internal/app/wake.go) treats a current binding
// carrying the "pull-only" capability as never wake-targetable -- see its
// doc comment. This file is the only place that mints such a binding.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/hollis-labs/tether/internal/registry"
)

// PullOnlyCapability is the sole supported capability for a published-
// local bridge binding leased over this HTTP surface. See file doc.
const PullOnlyCapability = "pull-only"

type bindingLeaseRequest struct {
	TargetURN string `json:"target_urn"`
	SessionID string `json:"session_id"`
	HostID    string `json:"host_id"`
	AttemptID string `json:"attempt_id"`
	// Capabilities must be exactly ["pull-only"]; see file doc.
	Capabilities []string `json:"capabilities"`
	TTLSeconds   int      `json:"ttl_seconds"`
}

type bindingRenewRequest struct {
	TTLSeconds int `json:"ttl_seconds"`
}

// handleBindingsCollection services /registry/bindings: POST leases a new
// published-local binding; GET lists every binding ever leased for
// ?target_urn= (audit view, newest generation first) or the single
// current one when ?current=true is also set.
func (s *Server) handleBindingsCollection(w http.ResponseWriter, r *http.Request) {
	if s.Registry == nil {
		writeError(w, http.StatusNotFound, CodeNotFound, "registry not configured")
		return
	}
	switch r.Method {
	case http.MethodPost:
		s.handleBindingLease(w, r)
	case http.MethodGet:
		s.handleBindingsList(w, r)
	default:
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed on /registry/bindings")
	}
}

func (s *Server) handleBindingLease(w http.ResponseWriter, r *http.Request) {
	var req bindingLeaseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "invalid request body: "+err.Error())
		return
	}
	if req.TargetURN == "" || req.SessionID == "" || req.HostID == "" || req.AttemptID == "" {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "target_urn, session_id, host_id and attempt_id are required")
		return
	}
	if len(req.Capabilities) != 1 || req.Capabilities[0] != PullOnlyCapability {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest,
			`capabilities must be exactly ["pull-only"] -- push-notified bridging over a caller-supplied webhook is not implemented; the bridge pulls its own mailbox instead`)
		return
	}
	// LeaseBinding itself performs no ownership check at all -- it
	// unconditionally mints the next generation for whatever target_urn is
	// named. Without a guard, any same-host caller who knows/guesses a
	// logical_agent_id could silence wake delivery for a CURRENTLY
	// RUNNING, healthy local session with one call (isPullOnly in
	// internal/app/wake.go would then permanently suppress its wake path
	// until an operator noticed and revoked the binding). This endpoint
	// may only supersede a binding that is ALREADY published-local (or
	// supersede nothing, for a never-bound target) -- taking over a
	// private-local/tether-hosted binding (a real Tether-managed session)
	// is refused, observably. Tether's OWN internal launch path
	// (session_lifecycle.go's leaseActorBinding) is deliberately NOT
	// subject to this guard -- a legitimate host reactivating a durable
	// actor that was previously pull-only-bound must be able to reclaim it
	// freely; only THIS external, self-asserted HTTP surface is
	// restricted. LeaseBindingUnlessVisibility checks the current binding
	// and mints the new one inside ONE transaction (registry/bindings.go)
	// -- a distinct review pass found the original check-then-lease
	// sequence here raced against Tether's own internal launch path
	// committing a fresh binding in the gap between the two calls, which
	// this closes rather than merely documenting.
	ttl := time.Duration(req.TTLSeconds) * time.Second
	b, err := s.Registry.LeaseBindingUnlessVisibility(r.Context(), req.TargetURN, req.SessionID, req.HostID, req.AttemptID, req.Capabilities, registry.VisibilityPublishedLocal, ttl,
		registry.VisibilityPrivateLocal, registry.VisibilityTetherHosted)
	if err != nil {
		if errors.Is(err, registry.ErrVisibilityConflict) {
			writeError(w, http.StatusConflict, CodeConflict,
				"target_urn is currently bound to a Tether-managed session; this endpoint cannot supersede it: "+err.Error())
			return
		}
		writeBindingError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, b)
}

func (s *Server) handleBindingsList(w http.ResponseWriter, r *http.Request) {
	target := r.URL.Query().Get("target_urn")
	if target == "" {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "target_urn query param required")
		return
	}
	if r.URL.Query().Get("current") == "true" {
		b, err := s.Registry.CurrentBinding(r.Context(), target)
		if err != nil {
			writeBindingError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, b)
		return
	}
	rows, err := s.Registry.ListBindingsForTarget(r.Context(), target)
	if err != nil {
		writeBindingError(w, err)
		return
	}
	if rows == nil {
		rows = []registry.RuntimeBinding{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"bindings": rows})
}

// handleBindingsItem services /registry/bindings/{binding_id}/renew and
// /registry/bindings/{binding_id}/revoke.
func (s *Server) handleBindingsItem(w http.ResponseWriter, r *http.Request) {
	if s.Registry == nil {
		writeError(w, http.StatusNotFound, CodeNotFound, "registry not configured")
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/registry/bindings/")
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 || parts[0] == "" {
		writeError(w, http.StatusNotFound, CodeNotFound, "binding id and action required")
		return
	}
	bindingID, action := parts[0], parts[1]
	switch action {
	case "renew":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed on .../renew")
			return
		}
		s.handleBindingRenew(w, r, bindingID)
	case "revoke":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed on .../revoke")
			return
		}
		s.handleBindingRevoke(w, r, bindingID)
	default:
		writeError(w, http.StatusNotFound, CodeNotFound, "unknown binding action "+action)
	}
}

func (s *Server) handleBindingRenew(w http.ResponseWriter, r *http.Request, bindingID string) {
	var req bindingRenewRequest
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, "invalid request body: "+err.Error())
			return
		}
	}
	ttl := time.Duration(req.TTLSeconds) * time.Second
	b, err := s.Registry.RenewBindingLease(r.Context(), bindingID, ttl)
	if err != nil {
		writeBindingError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, b)
}

func (s *Server) handleBindingRevoke(w http.ResponseWriter, r *http.Request, bindingID string) {
	if err := s.Registry.RevokeBinding(r.Context(), bindingID); err != nil {
		writeBindingError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeBindingError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, registry.ErrBindingNotFound):
		writeError(w, http.StatusNotFound, CodeNotFound, err.Error())
	case errors.Is(err, registry.ErrStaleGeneration):
		// The caller's binding has been fenced out by a newer generation
		// (T06) -- a real, expected outcome of concurrent-actor-session
		// handling, not a server error.
		writeError(w, http.StatusConflict, CodeConflict, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
	}
}
