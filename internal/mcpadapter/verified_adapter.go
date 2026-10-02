package mcpadapter

import (
	"context"

	"github.com/hollis-labs/tether/internal/app"
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
	a := NewWithDaemon(svc, dc, "verified-principal", p.Scopes)
	a.principal = &p
	a.SessionID = p.SessionID
	return a, nil
}
