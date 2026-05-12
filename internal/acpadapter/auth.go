package acpadapter

import (
	"encoding/json"
	"strings"
	"sync/atomic"
)

// Scope constants. ACP doesn't define scopes natively, but Mux mirrors
// `mux mcp` token + scope gating so editors can be wired with a
// least-privilege token. session.write covers create/launch/stop/etc.;
// no message scope MVP since ACP doesn't expose Mux messaging.
const (
	ScopeSessionWrite = "session.write"
)

// AuthGate enforces the token-bearer + scope authorization model.
// Token validation happens once via Authenticate(); subsequent
// authenticated requests gate on scope membership.
//
// Per the ACP spec, the editor calls `authenticate` with a method ID +
// body. Mux advertises a single method id "token" whose body is
// {"token": "<bearer>"}. Successful Authenticate() flips authed → true
// and unblocks scope-gated handlers.
//
// Concurrency: Authenticate() is called from the dispatcher's request
// goroutine; HasScope() is called from any handler goroutine. Use an
// atomic so the read path is lock-free.
type AuthGate struct {
	expectedToken string
	scopes        map[string]struct{}
	authed        atomic.Bool
}

// NewAuthGate constructs a gate with the configured token + allowed
// scope set. An empty token disables auth (development only — handlers
// behave as if Authenticate succeeded). An empty scope set with a
// non-empty token leaves all scope-gated handlers locked.
func NewAuthGate(token string, scopes []string) *AuthGate {
	scopeSet := make(map[string]struct{}, len(scopes))
	for _, s := range scopes {
		s = strings.TrimSpace(s)
		if s != "" {
			scopeSet[s] = struct{}{}
		}
	}
	g := &AuthGate{
		expectedToken: strings.TrimSpace(token),
		scopes:        scopeSet,
	}
	if g.expectedToken == "" {
		// Dev-only no-auth path: pre-flip so handlers don't reject.
		g.authed.Store(true)
	}
	return g
}

// AuthMethods returns the AuthMethod list the agent advertises in its
// initialize response. When auth is disabled (empty token) returns
// nil so the editor knows authentication is optional.
func (g *AuthGate) AuthMethods() []AuthMethod {
	if g.expectedToken == "" {
		return nil
	}
	return []AuthMethod{{
		ID:          "token",
		Name:        "Token",
		Description: "Bearer token via AGENT_MUX_ACP_TOKEN env or --token flag.",
	}}
}

// Authenticate validates AuthenticateParams and flips the gate on
// success. Returns an *RPCError suitable for direct return from a
// JSON-RPC handler on failure (invalid params, unsupported method,
// or token mismatch).
func (g *AuthGate) Authenticate(params AuthenticateParams) *RPCError {
	if g.expectedToken == "" {
		// Auth disabled — accept any payload as a dev-mode signal.
		return nil
	}
	if params.MethodID != "token" {
		return &RPCError{
			Code:    ErrCodeInvalidParams,
			Message: "unknown auth method: " + params.MethodID,
		}
	}
	var body AuthenticateTokenBody
	if len(params.Body) > 0 {
		if err := json.Unmarshal(params.Body, &body); err != nil {
			return &RPCError{
				Code:    ErrCodeInvalidParams,
				Message: "authenticate body must be {\"token\":\"...\"}: " + err.Error(),
			}
		}
	}
	if strings.TrimSpace(body.Token) != g.expectedToken {
		return &RPCError{
			Code:    ErrCodeInvalidParams,
			Message: "token mismatch",
		}
	}
	g.authed.Store(true)
	return nil
}

// IsAuthed reports whether Authenticate has been called successfully
// (or auth was disabled by passing an empty token at construction).
func (g *AuthGate) IsAuthed() bool {
	return g.authed.Load()
}

// HasScope reports whether the gate has authed AND the scope is in
// the configured set. Returns an *RPCError suitable for direct return
// from a handler when access is denied.
func (g *AuthGate) HasScope(scope string) *RPCError {
	if !g.IsAuthed() {
		return &RPCError{
			Code:    ErrCodeInvalidRequest,
			Message: "not authenticated; call authenticate first",
		}
	}
	if g.expectedToken == "" {
		// Dev-mode — no scope enforcement either.
		return nil
	}
	if _, ok := g.scopes[scope]; !ok {
		return &RPCError{
			Code:    ErrCodeInvalidRequest,
			Message: "missing required scope: " + scope,
		}
	}
	return nil
}
