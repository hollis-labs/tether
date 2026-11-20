package app

import (
	"context"
	"errors"
	"github.com/hollis-labs/tether/internal/session"
	"time"

	"github.com/hollis-labs/tether/internal/authissuer"
	"github.com/hollis-labs/tether/internal/gatewaylaunch"
	"github.com/hollis-labs/tether/internal/identity"
)

// startGatewayCredential runs after mintSessionCredential has applied the
// launcher's ceiling. Reverification on refresh preserves revocation and the
// original lease; neither a workspace value nor an agent JWT can mint here.
func (s *Service) startGatewayCredential(endpoint, sessionID, launchToken, runToken string) (string, func(), error) {
	if err := gatewaylaunch.ValidateURL(endpoint); err != nil {
		return "", nil, err
	}
	issuer, ok := s.AgentTokens.(interface {
		MintLocalSession(identity.Principal) (authissuer.Token, error)
	})
	if !ok {
		return "", nil, errors.New("gateway session issuer unavailable")
	}
	path, stop, err := gatewaylaunch.Start(func(ctx context.Context) (gatewaylaunch.Credential, error) {
		p, err := identity.NewStore(s.Store.DB()).Verify(ctx, launchToken)
		if err != nil || p.SessionID != sessionID {
			return gatewaylaunch.Credential{}, errors.New("gateway launch credential inactive")
		}
		tok, err := issuer.MintLocalSession(p)
		return gatewaylaunch.Credential{Token: tok.Token, ExpiresAt: tok.ExpiresAt}, err
	})
	if err != nil {
		return "", nil, err
	}
	if err := gatewaylaunch.BindRunToken(path, runToken); err != nil {
		stop()
		return "", nil, err
	}
	return path, stop, nil
}

// An uncertain shim submission retains its credential until the server-owned
// lifecycle proves completion. Do not turn a client cancellation into revocation.
func (s *Service) finishGatewayAfterSettlement(ctx context.Context, id string, stop func()) {
	defer stop()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		row, err := s.Store.GetSession(id)
		if err != nil || row == nil || session.State(row.State).Terminal() {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
