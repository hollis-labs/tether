package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"time"

	messaging "github.com/hollis-labs/libs/mesh/core/messaging"
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
	if s.cat() == nil || plan.TeamMember {
		return
	}
	agent, ok := s.cat().Agents[plan.CatalogAgent()]
	if !ok {
		return
	}
	plan.IdentityURN = agent.URN
	plan.MCPScopes = slices.Clone(agent.MCPScopes)
	plan.EditableAgents = slices.Clone(agent.EditableAgents)
}

// catalogEditableAgents returns the editable_agents allowlist of the plan's
// catalog agent: the ids its session's tether_agent_create / tether_agent_edit
// may touch. Like the URN it is read from the catalog agent only, never from an
// agent_file/agent_inline override, and a team member never takes it.
func (s *Service) catalogEditableAgents(plan *launch.Plan) []string {
	if s.cat() == nil || plan == nil || plan.TeamMember {
		return nil
	}
	agent, ok := s.cat().Agents[plan.CatalogAgent()]
	if !ok {
		return nil
	}
	return slices.Clone(agent.EditableAgents)
}

// launchEditableAgents is the editable_agents allowlist a launch saves into the
// session's MCP policy: the plan's copy (a worker plan carries it from
// applyCatalogIdentity) or, failing that, the catalog agent's. Both come from the
// catalog agent only, so this agrees with what withEditableAgents plants.
func (s *Service) launchEditableAgents(plan *launch.Plan) []string {
	if plan != nil && len(plan.EditableAgents) > 0 {
		return slices.Clone(plan.EditableAgents)
	}
	return s.catalogEditableAgents(plan)
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

// launcherID is the verified principal that asked for this launch, from the
// launch request's context; empty for an anonymous or internal launch. It
// becomes created_by on the session credential, the record of who launched the
// session (what adoption reads). mintSessionCredential sets the same field from
// the same context.
func launcherID(ctx context.Context) string {
	if parent, ok := identity.FromContext(ctx); ok {
		return parent.ID
	}
	return ""
}

// prepareWorkerIdentity mints the session-bound credential into spec and binds the agent's stable URN to
// the session, before the launch frame is sent. The returned undo releases both when the launch fails.
// A worker that does not advertise FeatureSessionCred keeps the static MCP entry; if the agent declares a
// URN the launch is refused instead, because an unbound session could never act as that identity.
func (s *Service) prepareWorkerIdentity(ctx context.Context, row *store.SessionRow, p *launch.Plan, spec *workerwire.LaunchSpec) (func(), error) {
	undo := func() {}
	mode := string(identity.Observe)
	if s.cat() != nil {
		mode = s.cat().Global.Identity.EffectiveMode()
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
	// A worker session may not hold more than the principal that launched it.
	wanted := scopes
	scopes, attribution, err := s.launchCeilingFor(ctx, wanted, narrowToLauncherWorker)
	if err == nil {
		err = s.refuseUnboundedWhenEnforced(attribution)
	}
	if err == nil && len(scopes) == 0 {
		// Nothing the profile needs survives the launcher's authority. Say so
		// here, as a refusal; the worker would only see "no agent token".
		err = launchScopeError{wanted: wanted}
	}
	if err != nil {
		revokeBinding()
		return undo, err
	}
	expires := time.Now().UTC().Add(sessionTokenTTL)
	self := "msg://session/local/" + row.ID
	// CreatedBy records who launched the session, as the hub-local path does
	// (mintSessionCredential). Without it every worker-hosted session credential
	// had created_by='' and could never be shown to belong to its launcher.
	pr := identity.Principal{ID: self, Kind: "session", SessionID: row.ID, Display: "Session " + row.ID, Scopes: scopes, ExpiresAt: &expires, Addresses: []string{self}, CreatedBy: launcherID(ctx), LaunchAttribution: string(attribution)}
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
	s.auditLaunchAttribution(ctx, row.ID, mode, attribution)
	spec.Credential, spec.MCPScopes = token, scopes
	revokeGrant := s.grantAgentTokens(ctx, row, spec, scopes)
	return func() {
		revokeBinding()
		revokeGrant()
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
	if s.AgentTokens != nil {
		if err := s.AgentTokens.RevokeSession(sessionID); err != nil {
			log.Printf("worker session %s: agent token revocation failed", sessionID)
		}
	}
	if s.Store == nil {
		return
	}
	if err := identity.NewStore(s.Store.DB()).RevokeSession(context.Background(), sessionID); err != nil {
		log.Printf("worker session %s: credential revocation failed", sessionID)
	}
}

// AgentTokenIssuer is the slice of the token issuer the session lifecycle
// uses: the launch grant bounds what the outpost may mint for a session, and
// RevokeSession deny-lists the sid when the session ends.
type AgentTokenIssuer interface {
	RegisterSessionGrant(sid, device string, scopes map[string][]string, expires time.Time) error
	RevokeSession(sid string) error
}

// grantAgentTokens registers the session's launch grant and tells the outpost
// which token files to keep. The scopes come from the launch policy (the
// agent's mcp_scopes, mapped into the tether.* vocabulary). It is best effort
// beside the existing session credential: a worker without FeatureAgentToken,
// or an issuer failure, leaves the launch as it was. The returned func undoes
// the grant when the launch fails.
func (s *Service) grantAgentTokens(ctx context.Context, row *store.SessionRow, spec *workerwire.LaunchSpec, mcpScopes []string) func() {
	noop := func() {}
	if s.AgentTokens == nil || !s.Workers.Supports(row.WorkerID, workerwire.FeatureAgentToken) {
		return noop
	}
	canonical, err := identity.CanonicalScopes(mcpScopes)
	if err != nil || len(canonical) == 0 {
		// The launch then falls to the degraded ssh path or fails; say why here, since the worker only sees "no token".
		log.Printf("worker session %s: no agent token granted: launch scopes %v are empty or outside the vocabulary", row.ID, mcpScopes)
		return noop
	}
	device, err := s.Workers.DeviceID(ctx, row.WorkerID)
	if err != nil {
		log.Printf("worker session %s: no agent token granted: worker device lookup failed", row.ID)
		return noop
	}
	grant := map[string][]string{"tether": canonical}
	if err := s.AgentTokens.RegisterSessionGrant(row.ID, device, grant, time.Now().Add(sessionTokenTTL)); err != nil {
		log.Printf("worker session %s: agent token grant failed", row.ID)
		return noop
	}
	spec.AgentTokens = []workerwire.AgentTokenSpec{{Aud: "tether", Scopes: canonical}}
	return func() {
		if err := s.AgentTokens.RevokeSession(row.ID); err != nil {
			log.Printf("worker session %s: agent token revocation failed", row.ID)
		}
	}
}
