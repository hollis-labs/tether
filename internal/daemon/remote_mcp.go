package daemon

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/hollis-labs/tether/internal/authissuer"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/store"
)

// Remote /mcp for bound agent tokens (CW-20261016-0160 H1; architect ruling remote_mcp_bound_agent_tokens_2026_10_16).
//
// A worker's agent reaches the hub's MCP gateway over HTTPS (loopback listener behind Tailscale Serve) with the
// session's own issuer-minted, session-bound agent token. Only that. The gateway then applies the session's saved
// policy, the H2 authority fingerprint, the limits and proxy capture exactly as for a local caller: this gate only
// decides who may reach it remotely. Everything else on /mcp stays exactly as before the gate existed:
//
//	stored device credential                      -> 404 (the route is local custody)   [existing chain]
//	no credential / garbage / operator token       -> 401                                [existing chain]
//	JWT, bad signature / expired / denied / aud    -> 401  (the verifier rejects it)
//	JWT, valid but not a bound agent token         -> 404
//	JWT, bound agent token, session gone or ended  -> 401
//	JWT, bound agent token, live session           -> the MCP gateway
//
// The Host and Origin allow-lists are enforced before this runs (RemoteHandler), and /p/ is deliberately not served.

// remoteMCPPath is the one remote MCP route.
const remoteMCPPath = "/mcp"

// maxRemoteMCPAuthorization bounds the Authorization header: an issued JWT is about 470 characters.
const maxRemoteMCPAuthorization = 4096

// sessionLookup is the part of the launch service the gate needs.
type sessionLookup interface {
	GetSession(id string) (*store.SessionRow, error)
}

type remoteMCP struct {
	verifier identity.Verifier
	sessions sessionLookup
	next     http.Handler
	audit    func(context.Context, identity.Observation) error
	mode     identity.Mode

	auth func() *remoteAuthority

	mu      sync.Mutex
	denials map[string]uint64
}

// RemoteMCPStatus says whether the remote /mcp gate is serving and, when it is not, why. Without the gate every remote
// /mcp request takes the ordinary remote chain and is refused as an unauthenticated caller, which looks the same as a bad
// token from outside; this is how an operator tells the two apart (CW-20261016-0160 canary).
type RemoteMCPStatus struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
}

// remoteMCPGate builds the gate once, or returns nil when the pieces it needs are not configured (then /mcp stays
// unavailable remotely, as before).
func (s *Server) remoteMCPGate() *remoteMCP {
	s.remoteMCPOnce.Do(func() {
		switch {
		case s.MCP == nil:
			s.remoteMCPReason = "no MCP handler: set daemon.mcp_endpoint.enabled: true (and keep modules.local_mcp on)"
		case s.Issuer == nil:
			s.remoteMCPReason = "no token issuer: the daemon is not issuing agent tokens"
		case s.Identity == nil:
			s.remoteMCPReason = "no identity store"
		}
		if s.remoteMCPReason != "" {
			return
		}
		sessions := s.RemoteMCPSessions
		if sessions == nil {
			if l, ok := s.Service.(sessionLookup); ok {
				sessions = l
			}
		}
		if sessions == nil {
			s.remoteMCPReason = "no session lookup for the gate"
			return
		}
		verifier, err := authissuer.NewVerifier(s.Identity, s.Issuer)
		if err != nil {
			s.remoteMCPReason = "issued-token verifier unavailable"
			return
		}
		s.remoteMCP = &remoteMCP{verifier: verifier, sessions: sessions, next: s.MCP, audit: s.recordIdentityObservation, auth: func() *remoteAuthority { return s.remoteAuth }, mode: identity.Enforce, denials: map[string]uint64{}}
	})
	return s.remoteMCP
}

// RemoteMCPStatus reports whether the gate is built. It is nil when no remote listener is configured, since the gate
// has no meaning then.
func (s *Server) RemoteMCPStatus() *RemoteMCPStatus {
	if !s.Config.RemoteListener.Enabled {
		return nil
	}
	if s.remoteMCPGate() != nil {
		return &RemoteMCPStatus{Available: true}
	}
	return &RemoteMCPStatus{Reason: s.remoteMCPReason}
}

// announceRemoteMCP logs, once at startup, whether remote /mcp is serving, so a gate that was never built is not silent.
func (s *Server) announceRemoteMCP() {
	st := s.RemoteMCPStatus()
	switch {
	case st == nil:
	case st.Available:
		slog.Info("remote MCP gate ready: bound agent tokens may reach /mcp on the remote listener")
	default:
		slog.Warn("remote MCP gate NOT built: /mcp on the remote listener refuses every caller", "reason", st.Reason)
	}
}

// RemoteMCPDenials reports how many remote /mcp requests the gate refused, by reason.
func (s *Server) RemoteMCPDenials() map[string]uint64 {
	g := s.remoteMCP
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make(map[string]uint64, len(g.denials))
	for k, v := range g.denials {
		out[k] = v
	}
	return out
}

// mcpMethod reports the methods streamable-HTTP MCP uses. Anything else on /mcp is not for the gate.
func mcpMethod(m string) bool {
	return m == http.MethodPost || m == http.MethodGet || m == http.MethodDelete
}

// jwtBearer returns the token when r carries exactly one Authorization: Bearer header holding a JWT-shaped value
// (three dot-separated segments). Anything else is not for this gate.
func jwtBearer(r *http.Request) (string, bool) {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return "", false
	}
	parts := strings.Fields(values[0])
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || strings.Count(parts[1], ".") != 2 {
		return "", false
	}
	return parts[1], true
}

func (g *remoteMCP) deny(w http.ResponseWriter, r *http.Request, reason, session string, status int, code string) {
	// ids only, never the token or the Authorization header.
	slog.Info("remote MCP denied", "reason", reason, "status", status, "method", r.Method, "session", cleanAuditField(session), "node", cleanAuditField(r.Header.Get("Tailscale-User-Login")))
	g.mu.Lock()
	g.denials[reason]++
	g.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", "Bearer")
	}
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"error":{"code":"` + code + `","message":"` + code + `"}}`))
}

func (g *remoteMCP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	token, _ := jwtBearer(r)
	if len(r.Header.Get("Authorization")) > maxRemoteMCPAuthorization {
		g.deny(w, r, "header_too_large", "", http.StatusUnauthorized, "unauthorized")
		return
	}
	p, err := g.verifier.Verify(r.Context(), token)
	if err != nil {
		if errors.Is(err, identity.ErrInvalidToken) {
			g.deny(w, r, "invalid_token", "", http.StatusUnauthorized, "unauthorized")
		} else {
			g.deny(w, r, "identity_unavailable", "", http.StatusServiceUnavailable, "identity_unavailable")
		}
		return
	}
	if !identity.IsBoundAgentToken(p) {
		g.deny(w, r, "not_bound_agent", p.SessionID, http.StatusNotFound, "remote_route_unavailable")
		return
	}
	row, err := g.sessions.GetSession(p.SessionID)
	switch {
	case errors.Is(err, store.ErrSessionNotFound):
		g.deny(w, r, "session_gone", p.SessionID, http.StatusUnauthorized, "unauthorized")
		return
	case err != nil:
		g.deny(w, r, "session_lookup_failed", p.SessionID, http.StatusServiceUnavailable, "identity_unavailable")
		return
	case terminalSession(row.State):
		g.deny(w, r, "session_ended", p.SessionID, http.StatusUnauthorized, "unauthorized")
		return
	}
	if r.Method == http.MethodPost && r.Header.Get("Mcp-Session-Id") == "" {
		g.auditInitialize(r, p, row)
	}
	g.serveMCP(w, r)
}

// serveMCP is the one place a request becomes "remote" for the MCP handler. The listener's Host and Origin allow-lists
// already ran in RemoteHandler; they run again here, as the same remoteAuthority value, so the handler has no path around
// them, and only then is the server-owned remote provenance attached (the gateway skips its local-authority check for it,
// and the MCP SDK's own localhost Host guard is disabled; see mcptransport.NewHandler).
func (g *remoteMCP) serveMCP(w http.ResponseWriter, r *http.Request) {
	auth := g.auth()
	if auth == nil {
		g.deny(w, r, "no_remote_authority", "", http.StatusForbidden, "remote_route_unavailable")
		return
	}
	if msg, status := auth.refusal(r); status != 0 {
		g.deny(w, r, "host_or_origin", "", status, msg)
		return
	}
	g.next.ServeHTTP(w, r.WithContext(identity.WithRemoteContext(r.Context())))
}

func terminalSession(state string) bool {
	switch state {
	case "completed", "failed", "killed":
		return true
	}
	return false
}

// auditInitialize records one identity_audit row per remote MCP session initialize: principal, session, the
// worker the session runs on, and the Tailscale Serve node identity when Serve forwarded one.
func (g *remoteMCP) auditInitialize(r *http.Request, p identity.Principal, row *store.SessionRow) {
	if g.audit == nil {
		return
	}
	node := cleanAuditField(r.Header.Get("Tailscale-User-Login"))
	if node == "" {
		node = "-"
	}
	worker := cleanAuditField(row.WorkerID)
	if worker == "" {
		worker = "-"
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Second)
	defer cancel()
	_ = g.audit(ctx, identity.Observation{
		At: time.Now().UTC(), PrincipalID: p.ID, SessionID: p.SessionID, Mode: g.mode, Authentication: "verified",
		Method: r.Method, Route: remoteMCPPath, Event: "remote_mcp_initialize worker=" + worker + " node=" + node,
	})
}

// cleanAuditField keeps a forwarded value short and printable; it is data, never trusted for a decision.
func cleanAuditField(v string) string {
	v = strings.TrimSpace(v)
	if len(v) > 128 {
		v = v[:128]
	}
	return strings.Map(func(r rune) rune {
		if r < 0x21 || r > 0x7e {
			return -1
		}
		return r
	}, v)
}
