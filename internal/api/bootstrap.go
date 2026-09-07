package api

// bootstrap.go — T08 (messaging vNext, CW-20260906-0039): the
// provider-neutral local bootstrap/registration helper the scope text
// calls for, usable at the launch/host boundary (e.g. by agent-setup).
// Composes three ALREADY-BUILT primitives behind one call rather than
// inventing new low-level mechanics: RegisterIdempotent (T02, owner-
// scoped idempotent registration), LeaseBinding (T02/T07, generation-
// fenced binding), and buildWhoAmI (T08, self-discovery) -- so a caller
// gets its resolved identity back in the exact same shape a follow-up
// GET /whoami would show.
//
// "Bootstrap and local messaging cannot require online Tether" (T02
// architecture note) is a requirement on the CALLING side, not this
// endpoint: a caller that can't reach Tether generates a purely local
// SESSION identity and proceeds without ever calling this endpoint. This
// endpoint is the online-Tether half of that story -- when reachable, it
// upgrades a bare local identity into a registered/bound one.
//
// Publication determines how much state this call actually touches:
//   - "" or "private-local" (the default): a one-off or private session.
//     No registry.Profile is minted, no binding is leased -- "a one-off
//     session need not manufacture a permanent actor record merely to
//     send a message" (architecture doc). Tether's OWN internal launch
//     path (session_lifecycle.go) remains the only way to mint a
//     private-local/tether-hosted binding; this external surface cannot.
//   - "published-local": the explicit-publication case. Idempotently
//     registers (RegisterIdempotent, keyed by logical_agent_id) and
//     leases a binding, capabilities locked to ["pull-only"] -- the only
//     capability actually supported anywhere in this codebase today
//     (push-webhook bridging is not implemented; see bindings.go).
//
// Idempotent fallback ("repeated hook calls"): a second call with the
// same session_id/host_id/attempt_id for an already-current
// published-local binding is a true no-op -- it does NOT mint a new,
// wasted generation. A call with a genuinely different attempt_id (e.g.
// the process actually restarted) is a legitimate reconnect and mints a
// new generation as normal, fenced against the prior one exactly like
// any other LeaseBinding call.

import (
	"encoding/json"
	"net/http"

	"github.com/hollis-labs/tether/internal/registry"
)

type bootstrapSessionRequest struct {
	// SessionID is the preassigned SESSION identity (required).
	SessionID string `json:"session_id"`
	// Intent is an informational passthrough (e.g. "preassigned",
	// "resume", "fresh") -- not validated against a closed set here;
	// agentkit's own SessionBootstrap vocabulary owns that.
	Intent string `json:"intent,omitempty"`
	// LogicalAgentID's presence is what distinguishes a durable actor
	// from a standalone, session-only boot. Empty means standalone.
	LogicalAgentID string `json:"logical_agent_id,omitempty"`
	DisplayName    string `json:"display_name,omitempty"`
	// ProviderID is accepted but not consumed by this endpoint --
	// provider/session mapping is Tether's own internal launch path's
	// job (internal/app's UpsertSessionProviderMapping), which requires
	// a real sessions-table row this external surface does not assume
	// exists. Accepting and ignoring it (rather than rejecting the
	// field) is what makes the "missing provider ID" fixture a non-event
	// either way.
	ProviderID string `json:"provider_id,omitempty"`
	// Publication: "" / "private-local" (default) or "published-local".
	Publication string `json:"publication,omitempty"`
	HostID      string `json:"host_id,omitempty"`
	AttemptID   string `json:"attempt_id,omitempty"`
	Substrate   string `json:"substrate,omitempty"`
}

type bootstrapSessionResponse struct {
	whoAmIResponse
	// RegisteredNow is true only when this call minted a brand-new
	// registry.Profile (RegisterIdempotent's created flag) -- false on a
	// repeated hook call that found an existing registration.
	RegisteredNow bool `json:"registered_now,omitempty"`
	// ReusedBinding is true when an idempotent repeat call found an
	// already-current, matching binding and skipped leasing a new
	// generation.
	ReusedBinding bool `json:"reused_binding,omitempty"`
}

func (s *Server) registerBootstrapRoutes(mux *http.ServeMux) {
	if s.Registry == nil {
		return
	}
	mux.HandleFunc("/bootstrap/session", s.handleBootstrapSession)
}

func (s *Server) handleBootstrapSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
		return
	}
	var req bootstrapSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "invalid request body: "+err.Error())
		return
	}
	if req.SessionID == "" {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "session_id is required")
		return
	}

	// Standalone, session-only boot: no logical_agent_id means no
	// durable actor is being claimed. Report self-discovery for the
	// exact-session URN and stop -- no registry write, no binding.
	if req.LogicalAgentID == "" {
		sessionURN := "msg://session/agent-mux/" + req.SessionID
		writeJSON(w, http.StatusCreated, bootstrapSessionResponse{whoAmIResponse: s.buildWhoAmI(r, sessionURN)})
		return
	}

	target := registry.LogicalAgentBindingTarget(req.LogicalAgentID)
	var out bootstrapSessionResponse

	if req.Publication != "published-local" {
		// Private-local (default) durable actor: identity is claimed by
		// naming, not by registering or binding -- Tether's own internal
		// launch path owns minting a real binding for it later.
		out.whoAmIResponse = s.buildWhoAmI(r, target)
		writeJSON(w, http.StatusCreated, out)
		return
	}

	substrate := req.Substrate
	if substrate == "" {
		substrate = "agent-setup"
	}
	displayName := req.DisplayName
	if displayName == "" {
		displayName = req.LogicalAgentID
	}
	profile, created, err := s.Registry.RegisterIdempotent(r.Context(), registry.KindAgent,
		registry.Profile{DisplayName: displayName}, substrate, req.LogicalAgentID)
	if err != nil {
		writeRegistryError(w, err)
		return
	}
	out.RegisteredNow = created

	hostID := req.HostID
	if hostID == "" {
		hostID = "external-bootstrap"
	}
	attemptID := req.AttemptID
	if attemptID == "" {
		attemptID = req.SessionID
	}

	current, currErr := s.Registry.CurrentBinding(r.Context(), target)
	switch {
	case currErr == nil && current.SessionID == req.SessionID && current.HostID == hostID && current.AttemptID == attemptID:
		// Repeated hook call for the exact same attempt: idempotent
		// no-op, do not mint a redundant generation.
		out.ReusedBinding = true
	default:
		if !s.refuseToSupersedeManagedSession(w, r, target) {
			return
		}
		if _, err := s.Registry.LeaseBinding(r.Context(), target, req.SessionID, hostID, attemptID,
			[]string{PullOnlyCapability}, registry.VisibilityPublishedLocal, 0); err != nil {
			writeBindingError(w, err)
			return
		}
	}

	// buildWhoAmI(target) reports binding/groups/session for the binding
	// namespace (LogicalAgentBindingTarget, keyed by the raw
	// logical_agent_id -- the same URN wake resolution actually uses).
	// It CANNOT discover the profile RegisterIdempotent just
	// (re)confirmed: that call mints/reuses its own, independently-keyed
	// msg://agent/agent-mux/agt_<random> URN, a different identity
	// namespace from the binding target by design (T02: registry AgentID
	// vs. the pre-existing LogicalAgentID convention were never merged --
	// reconciling them is a larger identity question out of this task's
	// scope). Attaching profile from RegisterIdempotent's own return
	// value directly is the honest fix, not a workaround: a lookup
	// against target would silently return nothing every time, not a
	// transient miss.
	out.whoAmIResponse = s.buildWhoAmI(r, target)
	out.Profile = &profile
	writeJSON(w, http.StatusCreated, out)
}
