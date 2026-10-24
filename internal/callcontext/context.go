// Package callcontext carries secret-free, daemon-resolved call attribution.
// A snapshot describes attribution, never grants authorization to a caller.
package callcontext

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
)

type Snapshot struct {
	// Verified is identity.Principal.Verified: a bearer credential resolved to
	// a principal row, of any kind. SessionResolved is the narrower fact that
	// the principal is a session whose row was found; session-scoped fields
	// (SessionID, workstream, launch, project, agent) are trustworthy only
	// when it is true.
	Verified        bool   `json:"verified"`
	SessionResolved bool   `json:"session_resolved,omitempty"`
	Source          string `json:"source,omitempty"`
	PrincipalID     string `json:"principal_id,omitempty"`
	PrincipalKind   string `json:"principal_kind,omitempty"`
	SessionID       string `json:"session_id,omitempty"`
	AgentURN        string `json:"agent_urn,omitempty"`
	WorkstreamID    string `json:"workstream_id,omitempty"`
	TaskID          string `json:"task_id,omitempty"`
	LaunchID        string `json:"launch_id,omitempty"`
	ProjectID       string `json:"project_id,omitempty"`
	LogicalAgentID  string `json:"logical_agent_id,omitempty"`
}

type contextKey struct{}
type claimKey struct{}
type clientLabelKey struct{}
type mcpSessionKey struct{}

func WithClaimedSession(ctx context.Context, sessionID string) context.Context {
	return context.WithValue(ctx, claimKey{}, sessionID)
}

func ClaimedSession(ctx context.Context) string {
	id, _ := ctx.Value(claimKey{}).(string)
	return id
}

func WithSnapshot(ctx context.Context, s Snapshot) context.Context {
	return context.WithValue(ctx, contextKey{}, s)
}

func FromContext(ctx context.Context) (Snapshot, bool) {
	s, ok := ctx.Value(contextKey{}).(Snapshot)
	return s, ok
}

// WithClientLabel carries the caller's self-declared label. It is a claim.
func WithClientLabel(ctx context.Context, label string) context.Context {
	return context.WithValue(ctx, clientLabelKey{}, label)
}

func ClientLabel(ctx context.Context) string {
	label, _ := ctx.Value(clientLabelKey{}).(string)
	return label
}

// WithMCPSession carries a short hash of the MCP transport session handle.
func WithMCPSession(ctx context.Context, hash string) context.Context {
	return context.WithValue(ctx, mcpSessionKey{}, hash)
}

func MCPSession(ctx context.Context) string {
	hash, _ := ctx.Value(mcpSessionKey{}).(string)
	return hash
}

// ValidClientLabel reports whether s is an acceptable client label: 1-64 of
// letters, digits and . _ : @ / -. Anything else is dropped, not repaired.
func ValidClientLabel(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '_', r == ':', r == '@', r == '/', r == '-':
		default:
			return false
		}
	}
	return true
}

// WithTelemetryHints attaches two observation-only hints from a request's HTTP
// headers (CW-20261024-0048). Neither grants anything and neither is read by
// policy.
//
//   - X-Tether-Client-Label: the caller's self-declared name. Operator-token
//     clients share one verified principal, so without this they are one blob in
//     telemetry. It is a claim; an invalid value is dropped, not repaired.
//   - Mcp-Session-Id: reduced to a short hash so one connection's calls can be
//     grouped (and linked to the search that preceded them) without storing the
//     handle itself.
//
// The MCP SDK runs tool handlers on a session-level context, so these must be
// read from the per-request headers the SDK exposes (RequestExtra.Header), not
// from the HTTP request's context.
func WithTelemetryHints(ctx context.Context, h http.Header) context.Context {
	if h == nil {
		return ctx
	}
	if label := h.Get("X-Tether-Client-Label"); ValidClientLabel(label) {
		ctx = WithClientLabel(ctx, label)
	}
	if id := h.Get("Mcp-Session-Id"); id != "" {
		sum := sha256.Sum256([]byte(id))
		ctx = WithMCPSession(ctx, hex.EncodeToString(sum[:6]))
	}
	return ctx
}
