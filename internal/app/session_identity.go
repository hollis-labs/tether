package app

import (
	"context"
	"fmt"
	"github.com/hollis-labs/tether/internal/identity"
	"slices"
)

var workerScopes = []string{"session.write", "message.write", "catalog.write"}

func (s *Service) mintSessionCredential(ctx context.Context, sessionID string) (string, error) {
	if s.Catalog != nil && s.Catalog.Global.Identity.EffectiveMode() == string(identity.Off) {
		return "", nil
	}
	p := identity.Principal{ID: "msg://session/local/" + sessionID, Kind: "session", SessionID: sessionID, Display: "Session " + sessionID,
		Scopes: slices.Clone(workerScopes), Addresses: []string{"msg://session/local/" + sessionID}}
	if parent, ok := identity.FromContext(ctx); ok {
		p.CreatedBy = parent.ID
		if !slices.Contains(parent.Scopes, "*") {
			p.Scopes = nil
			for _, scope := range workerScopes {
				if slices.Contains(parent.Scopes, scope) {
					p.Scopes = append(p.Scopes, scope)
				}
			}
		}
	}
	token, err := identity.NewStore(s.Store.DB()).Mint(ctx, p)
	if err != nil {
		return "", fmt.Errorf("mint session credential: %w", err)
	}
	return token, nil
}
