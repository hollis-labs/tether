package mcpadapter

import (
	"context"
	"slices"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/callcontext"
	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/identity"
)

// NewVerifiedAdapter binds an isolated daemon view to middleware-verified
// identity. No client session/scopes flag is consulted. Native handlers reuse
// the daemon's Service or its existing API client, never another store/runtime.
// The transport must reverify credentials on every request before dispatch.
func NewVerifiedAdapter(ctx context.Context, svc *app.Service, dc *client.Client) (*Adapter, error) {
	p, ok := identity.FromContext(ctx)
	if !ok || p.ID == "" {
		return nil, identity.ErrInvalidToken
	}
	if dc != nil {
		if svc == nil || svc.Store == nil {
			return nil, identity.ErrInvalidToken
		}
		actual, err := dc.VerifiedPrincipal(ctx, identity.NewStore(svc.Store.DB()))
		if err != nil {
			return nil, err
		}
		wantedScopes, actualScopes := slices.Clone(p.Scopes), slices.Clone(actual.Scopes)
		slices.Sort(wantedScopes)
		slices.Sort(actualScopes)
		if actual.ID != p.ID || actual.Kind != p.Kind || actual.SessionID != p.SessionID || !slices.Equal(actualScopes, wantedScopes) {
			return nil, identity.ErrInvalidToken
		}
	}
	a := NewWithDaemon(svc, dc, "verified-principal", p.Scopes)
	a.principal = &p
	a.SessionID = p.SessionID
	return a, nil
}

// verifiedCallerContext replaces any inbound attribution with the daemon's
// resolver result. SDK native dispatch binds the admitted view principal here;
// neither an SDK metadata claim nor a cached stdio lookup establishes identity.
func (a *Adapter) verifiedCallerContext(ctx context.Context) context.Context {
	ctx = identity.WithPrincipal(ctx, *a.principal)
	ctx = callcontext.WithClaimedSession(ctx, a.principal.SessionID)
	var sessions api.CallerSessionLookup
	var bindings api.CallerBindingLookup
	if a.svc != nil {
		if a.svc.Store != nil {
			sessions = a.svc.Store
		}
		if a.svc.Registry != nil {
			bindings = a.svc.Registry
		}
	}
	return callcontext.WithSnapshot(ctx, api.ResolveCallerContext(ctx, sessions, bindings))
}
