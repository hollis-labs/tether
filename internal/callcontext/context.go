// Package callcontext carries secret-free, daemon-resolved call attribution.
// A snapshot describes attribution, never grants authorization to a caller.
package callcontext

import "context"

type Snapshot struct {
	Verified       bool   `json:"verified"`
	PrincipalID    string `json:"principal_id,omitempty"`
	PrincipalKind  string `json:"principal_kind,omitempty"`
	SessionID      string `json:"session_id,omitempty"`
	AgentURN       string `json:"agent_urn,omitempty"`
	WorkstreamID   string `json:"workstream_id,omitempty"`
	LaunchID       string `json:"launch_id,omitempty"`
	ProjectID      string `json:"project_id,omitempty"`
	LogicalAgentID string `json:"logical_agent_id,omitempty"`
}

type contextKey struct{}
type claimKey struct{}

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
