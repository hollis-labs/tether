package api

// whoami.go — T08 (messaging vNext, CW-20260906-0039): self-discovery.
// The architecture doc's own requirement, never built in T01-T07:
// "Expose self-discovery returning the caller's actual address, identity
// mappings, memberships, host, and negotiated capabilities."
//
// Same-host, self-asserted trust model throughout (ADR 0045): the caller
// names the URN it claims to be via ?urn=; whoami answers "what does
// Tether currently know about that identity," it does not verify the
// claim. Every section is best-effort/independent -- a caller that isn't
// registry-registered, isn't bound, and isn't a live local session still
// gets a 200 with an otherwise-empty response, not a 404, since "you
// exist as a bare URN with no further Tether state" is itself a valid,
// informative answer (matches the architecture's "A one-off session need
// not manufacture a permanent actor record merely to send a message").

import (
	"net/http"

	"github.com/hollis-labs/agentkit/agentsessions"
	messaging "github.com/hollis-labs/go-messaging"

	"github.com/hollis-labs/tether/internal/registry"
)

type whoAmIResponse struct {
	URN     string             `json:"urn"`
	Profile *registry.Profile  `json:"profile,omitempty"`
	Binding *whoAmIBinding     `json:"binding,omitempty"`
	Groups  []registry.Profile `json:"groups,omitempty"`
	Session *whoAmISession     `json:"session,omitempty"`
}

type whoAmIBinding struct {
	ID           string   `json:"id"`
	SessionID    string   `json:"session_id"`
	HostID       string   `json:"host_id"`
	Generation   int64    `json:"generation"`
	Capabilities []string `json:"capabilities,omitempty"`
	Visibility   string   `json:"visibility"`
}

// whoAmISession is the "host and negotiated capabilities" half of the
// architecture's requirement -- populated only when urn currently
// resolves to a session THIS daemon's runtime manager is actually
// running (agentsessions.Manager, via RuntimeHealth), not a bare
// registry/binding record.
type whoAmISession struct {
	SessionID string `json:"session_id"`
	// Host is a fixed literal, not a lookup: Tether has no multi-host
	// clustering model within one daemon instance (ADR 0045) -- every
	// session a given daemon runs IS that one host, matching
	// internal/app's own localHostID convention for the same reason.
	Host         string   `json:"host"`
	Capabilities []string `json:"capabilities,omitempty"`
}

func (s *Server) registerWhoAmIRoutes(mux *http.ServeMux) {
	if s.Registry == nil && s.Service == nil {
		return
	}
	mux.HandleFunc("/whoami", s.handleWhoAmI)
}

func (s *Server) handleWhoAmI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
		return
	}
	urn := r.URL.Query().Get("urn")
	if urn == "" {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "urn query param required")
		return
	}
	if _, err := messaging.ParseURN(urn); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "invalid urn: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.buildWhoAmI(r, urn))
}

// buildWhoAmI aggregates every independent, best-effort section of a
// self-discovery response for urn. Shared with bootstrap.go's
// handleBootstrapSession (T08) so a bootstrap call's response is provably
// the SAME shape/data a plain GET /whoami would return for the identity
// it just resolved, not a second, divergent projection. Assumes urn has
// already been validated as parseable.
func (s *Server) buildWhoAmI(r *http.Request, urn string) whoAmIResponse {
	out := whoAmIResponse{URN: urn}
	addr, err := messaging.ParseURN(urn)
	if err != nil {
		return out
	}

	if s.Registry != nil {
		if p, err := s.Registry.Lookup(r.Context(), urn); err == nil {
			out.Profile = &p
		}
		if b, err := s.Registry.CurrentBinding(r.Context(), urn); err == nil {
			out.Binding = &whoAmIBinding{
				ID: b.ID, SessionID: b.SessionID, HostID: b.HostID, Generation: b.Generation,
				Capabilities: b.Capabilities, Visibility: string(b.Visibility),
			}
		}
		if groups, err := s.Registry.ListGroupsForMember(r.Context(), urn); err == nil && len(groups) > 0 {
			out.Groups = groups
		}
	}

	if s.Service != nil {
		if sess := s.resolveWhoAmISession(r, addr); sess != nil {
			out.Session = sess
		}
	}
	return out
}

// resolveWhoAmISession finds a live LOCAL session for addr, reusing the
// exact binding-first/legacy-fallback resolution T06 already built for
// wake targeting (Service.ResolveActorSession) rather than a second,
// divergent lookup -- so whoami reports the SAME session a notify call
// would actually reach.
func (s *Server) resolveWhoAmISession(r *http.Request, addr messaging.Address) *whoAmISession {
	var sessionID string
	switch addr.Kind {
	case messaging.KindSession:
		sessionID = addr.ID
	case messaging.KindAgent:
		id, err := s.Service.ResolveActorSession(r.Context(), addr.ID)
		if err != nil || id == "" {
			return nil
		}
		sessionID = id
	default:
		return nil
	}
	health, ok := s.Service.RuntimeHealth(sessionID)
	if !ok {
		return nil
	}
	return &whoAmISession{
		SessionID:    sessionID,
		Host:         "local",
		Capabilities: negotiatedCapabilities(health.Caps),
	}
}

// negotiatedCapabilities projects agentkit's Capabilities flag struct to
// a stable, minimal string list rather than echoing the raw struct --
// keeps the wire contract independent of agentkit's own field set.
func negotiatedCapabilities(c agentsessions.Capabilities) []string {
	var out []string
	add := func(has bool, name string) {
		if has {
			out = append(out, name)
		}
	}
	add(c.PTY, "pty")
	add(c.StreamingStdio, "streaming_stdio")
	add(c.JsonRpcStdio, "json_rpc_stdio")
	add(c.ServeHTTP, "serve_http")
	add(c.Resize, "resize")
	add(c.ProviderSessionID, "provider_session_id")
	add(c.CheckpointResume, "checkpoint_resume")
	return out
}
