package api

import (
	"context"
	"net/http"

	"github.com/hollis-labs/tether/internal/callcontext"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/store"
)

type CallerSessionLookup interface {
	GetSession(string) (*store.SessionRow, error)
}

type CallerBindingLookup interface {
	CurrentBinding(context.Context, string) (registry.RuntimeBinding, error)
}

// ResolveCallerContext uses only the middleware-verified principal as selector.
// Lookup failures degrade attribution, not observe-mode availability. A binding
// establishes agent attribution only when it names this exact verified session.
func ResolveCallerContext(ctx context.Context, sessions CallerSessionLookup, bindings CallerBindingLookup) callcontext.Snapshot {
	p, ok := identity.FromContext(ctx)
	if !ok {
		return callcontext.Snapshot{}
	}
	out := callcontext.Snapshot{Source: "daemon", PrincipalID: p.ID, PrincipalKind: p.Kind}
	if p.Kind != "session" || p.SessionID == "" || sessions == nil {
		return out
	}
	row, err := sessions.GetSession(p.SessionID)
	if err != nil || row == nil || row.ID != p.SessionID {
		return out
	}
	out.Verified = true
	out.SessionID, out.WorkstreamID = row.ID, row.WorkstreamID.String
	out.LaunchID, out.ProjectID, out.LogicalAgentID = row.LaunchID, row.ProjectID, row.LogicalAgentID
	if bindings != nil && row.LogicalAgentID != "" {
		target := registry.LogicalAgentBindingTarget(row.LogicalAgentID)
		if binding, err := bindings.CurrentBinding(ctx, target); err == nil && binding.SessionID == row.ID && binding.TargetURN == target {
			out.AgentURN = binding.TargetURN
		}
	}
	return out
}

func (s *Server) registerCallerContextRoutes(router *http.ServeMux) {
	router.HandleFunc("/auth/context", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
			return
		}
		writeJSON(w, http.StatusOK, ResolveCallerContext(r.Context(), s.Service, s.Registry))
	})
}
