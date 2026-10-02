package app

import (
	"context"
	"fmt"
	"github.com/hollis-labs/tether/internal/identity"
	"slices"
	"time"
)

const sessionTokenTTL = 7 * 24 * time.Hour

var workerScopes = []string{"session.write", "message.write", "catalog.write"}

func (s *Service) mintSessionCredential(ctx context.Context, sessionID string) (string, error) {
	if s.Catalog != nil && s.Catalog.Global.Identity.EffectiveMode() == string(identity.Off) {
		return "", nil
	}
	expires := time.Now().UTC().Add(sessionTokenTTL)
	p := identity.Principal{ExpiresAt: &expires, ID: "msg://session/local/" + sessionID, Kind: "session", SessionID: sessionID, Display: "Session " + sessionID,
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
	token, err := identity.NewStore(s.Store.DB()).MintSessionForLaunch(ctx, p)
	if err != nil {
		return "", fmt.Errorf("mint session credential: %w", err)
	}
	return token, nil
}

// sessionLaunchGate serializes preparation and minting for one session, including
// observe-mode launches that cannot mint. Entries disappear after the last waiter.
type sessionLaunchGate struct {
	slot chan struct{}
	refs int
}

func (s *Service) lockSessionLaunch(ctx context.Context, id string) (func(), error) {
	s.launchMu.Lock()
	if s.launches == nil {
		s.launches = make(map[string]*sessionLaunchGate)
	}
	gate := s.launches[id]
	if gate == nil {
		gate = &sessionLaunchGate{slot: make(chan struct{}, 1)}
		s.launches[id] = gate
	}
	gate.refs++
	s.launchMu.Unlock()
	dropRef := func() {
		s.launchMu.Lock()
		defer s.launchMu.Unlock()
		gate.refs--
		if gate.refs == 0 {
			delete(s.launches, id)
		}
	}
	select {
	case gate.slot <- struct{}{}:
		return func() { <-gate.slot; dropRef() }, nil
	case <-ctx.Done():
		dropRef()
		return nil, ctx.Err()
	}
}
