package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"time"

	messaging "github.com/hollis-labs/substrate/mesh/messaging"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/launchprofile"
	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/session"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/workerwire"
)

// ErrIdentityInUse is returned when an agent's stable URN is already leased by
// another live session. The launch fails closed rather than stealing the URN.
var ErrIdentityInUse = errors.New("stable agent identity is already bound to another live session")

// applyCatalogIdentity copies the stable URN and credential scopes from the
// CATALOG agent onto a worker plan. It never reads an agent_file/agent_inline
// override, and a team member (derived "<agent>#<slot>" id) never takes the
// base agent's URN.
func (s *Service) applyCatalogIdentity(plan *launch.Plan) {
	if s.Catalog == nil || plan.TeamMember {
		return
	}
	agent, ok := s.Catalog.Agents[plan.CatalogAgent()]
	if !ok {
		return
	}
	plan.IdentityURN = agent.URN
	plan.MCPScopes = slices.Clone(agent.MCPScopes)
}

// SessionIdentityURN returns the stable URN a session was launched to act as, if any. It is only a
// claim: ResolveCallerContext trusts it solely when the registry binding for that URN names the session.
func (s *Service) SessionIdentityURN(sessionID string) string {
	plan, err := s.Store.GetLaunchPlan(sessionID)
	if err != nil || plan == nil {
		return ""
	}
	return plan.IdentityURN
}

// prepareWorkerIdentity mints the session-bound credential into spec and binds the agent's stable URN to
// the session, before the launch frame is sent. The returned undo releases both when the launch fails.
// A worker that does not advertise FeatureSessionCred keeps the static MCP entry; if the agent declares a
// URN the launch is refused instead, because an unbound session could never act as that identity.
func (s *Service) prepareWorkerIdentity(ctx context.Context, row *store.SessionRow, p *launch.Plan, spec *workerwire.LaunchSpec) (func(), error) {
	undo := func() {}
	mode := string(identity.Observe)
	if s.Catalog != nil {
		mode = s.Catalog.Global.Identity.EffectiveMode()
	}
	if s.Workers == nil || !s.Workers.Supports(row.WorkerID, workerwire.FeatureSessionCred) {
		if p.IdentityURN != "" {
			return undo, fmt.Errorf("agent declares urn %s but worker does not support session credentials; upgrade tether-worker", p.IdentityURN)
		}
		return undo, nil
	}
	if mode == string(identity.Off) {
		if p.IdentityURN != "" {
			return undo, fmt.Errorf("agent declares urn %s but identity mode is off", p.IdentityURN)
		}
		return undo, nil
	}
	var bindingID string
	if p.IdentityURN != "" {
		b, err := s.bindStableIdentity(ctx, row, p.IdentityURN)
		if err != nil {
			return undo, err
		}
		bindingID = b.ID
	}
	revokeBinding := func() {
		if bindingID != "" && s.Registry != nil {
			if err := s.Registry.RevokeBinding(context.Background(), bindingID); err != nil {
				log.Printf("worker session %s: release identity binding failed: %v", row.ID, err)
			}
		}
	}
	scopes := slices.Clone(p.MCPScopes)
	if len(scopes) == 0 {
		scopes = slices.Clone(launchprofile.DefaultMCPScopes)
	}
	expires := time.Now().UTC().Add(sessionTokenTTL)
	self := "msg://session/local/" + row.ID
	pr := identity.Principal{ID: self, Kind: "session", SessionID: row.ID, Display: "Session " + row.ID, Scopes: scopes, ExpiresAt: &expires, Addresses: []string{self}}
	if row.LogicalAgentID != "" {
		agent := registry.LogicalAgentBindingTarget(row.LogicalAgentID)
		if addr, err := messaging.ParseURN(agent); err == nil && addr.URN() == agent {
			pr.Addresses = append(pr.Addresses, agent)
		}
	}
	if p.IdentityURN != "" {
		pr.Addresses = append(pr.Addresses, p.IdentityURN)
	}
	token, err := identity.NewStore(s.Store.DB()).MintSessionForLaunch(ctx, pr)
	if err != nil {
		revokeBinding()
		return undo, fmt.Errorf("mint worker session credential: %w", err)
	}
	spec.Credential, spec.MCPScopes = token, scopes
	return func() {
		revokeBinding()
		if err := identity.NewStore(s.Store.DB()).RevokeSession(context.Background(), row.ID); err != nil {
			log.Printf("worker session %s: credential revocation failed", row.ID)
		}
	}, nil
}

// bindStableIdentity leases urn to the session. The URN must already be registered (it is looked up,
// never created here). A live lease held by a different session fails closed with ErrIdentityInUse; a
// lease left by an ended session is released first.
func (s *Service) bindStableIdentity(ctx context.Context, row *store.SessionRow, urn string) (registry.RuntimeBinding, error) {
	if s.Registry == nil {
		return registry.RuntimeBinding{}, fmt.Errorf("agent declares urn %s but the registry is unavailable", urn)
	}
	if _, err := s.Registry.Lookup(ctx, urn); err != nil {
		return registry.RuntimeBinding{}, fmt.Errorf("agent urn %s is not registered: %w", urn, err)
	}
	if cur, err := s.Registry.CurrentBinding(ctx, urn); err == nil && cur.SessionID != row.ID {
		other, getErr := s.Store.GetSession(cur.SessionID)
		if getErr == nil && !session.State(other.State).Terminal() {
			return registry.RuntimeBinding{}, fmt.Errorf("%w: %s is held by session %s", ErrIdentityInUse, urn, cur.SessionID)
		}
		if err := s.Registry.RevokeBinding(ctx, cur.ID); err != nil {
			return registry.RuntimeBinding{}, err
		}
	}
	b, err := s.Registry.LeaseBindingUnlessVisibility(ctx, urn, row.ID, row.WorkerID, row.ID, nil, registry.VisibilityTetherHosted, 0,
		registry.VisibilityPrivateLocal, registry.VisibilityPublishedLocal, registry.VisibilityTetherHosted)
	if errors.Is(err, registry.ErrVisibilityConflict) {
		return registry.RuntimeBinding{}, fmt.Errorf("%w: %s", ErrIdentityInUse, urn)
	}
	return b, err
}

// releaseWorkerIdentity revokes a worker session's credential and identity bindings once it has ended.
func (s *Service) releaseWorkerIdentity(sessionID string) {
	if s.Store == nil {
		return
	}
	if err := identity.NewStore(s.Store.DB()).RevokeSession(context.Background(), sessionID); err != nil {
		log.Printf("worker session %s: credential revocation failed", sessionID)
	}
}
