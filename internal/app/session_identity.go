package app

import (
	"context"
	"fmt"
	"github.com/google/uuid"
	messaging "github.com/hollis-labs/libs/mesh/core/messaging"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/store"
	"slices"
	"time"
)

const sessionTokenTTL = 7 * 24 * time.Hour

var workerScopes = []string{"session.write", "message.write", "catalog.write"}

// narrowToLauncher returns the part of granted that the launching principal
// itself holds, so a launch can never hand a session more authority than its
// launcher has. A launcher holding "*" keeps all of granted. A launch with no
// principal in its context (anonymous or daemon-internal, which only happens
// when identity is observe or off) has nothing to inherit from and keeps
// granted as the catalog or profile set it; it does not widen anything, since
// granted is itself the ceiling. Both the hub-local and the worker credential
// paths use this, so they cannot drift apart.
//
// The comparison is vocabulary-aware (identity.ScopeGranted) for a launcher that
// reached the hub with an issued agent or session JWT, and, on the worker launch
// path only (deviceVocab), for a paired device as well. A profile ceiling is
// written in the legacy child names (message.write ...), while a launcher holds
// canonical tether.* scopes or bare device scopes (operate, maintain), and an
// exact string match emptied the ceiling: the worker got no session agent token
// and the launch failed (#470 for agents; CW-20261020-0048 for a Tachyon device
// launching a general worker). The match only ever keeps an entry of granted
// that the launcher already holds, exactly or by the documented containment
// (operate holds the messages, groups, sessions, teams and ai child scopes;
// maintain holds registry and catalog), and legacy-to-canonical is one way, so
// nothing is added or widened.
//
// The hub-local path keeps comparing a device exactly: its child gets no legacy
// scopes (docs/device-pairing.md, CW-20261010-0173).
func narrowToLauncher(ctx context.Context, granted []string) []string {
	return narrowToLauncherAs(ctx, granted, false)
}

// narrowToLauncherWorker is narrowToLauncher for the worker launch path, where a
// paired device's bare scopes are compared by containment.
func narrowToLauncherWorker(ctx context.Context, granted []string) []string {
	return narrowToLauncherAs(ctx, granted, true)
}

func narrowToLauncherAs(ctx context.Context, granted []string, deviceVocab bool) []string {
	parent, ok := identity.FromContext(ctx)
	if !ok || slices.Contains(parent.Scopes, "*") {
		return slices.Clone(granted)
	}
	if issuedAgentCredential(parent) || deviceVocab && parent.Kind == "device" {
		return retainHeld(granted, parent.Scopes)
	}
	var out []string
	for _, scope := range granted {
		if slices.Contains(parent.Scopes, scope) {
			out = append(out, scope)
		}
	}
	return out
}

// issuedAgentCredential reports whether p is an agent or session principal authenticated by an issued JWT.
func issuedAgentCredential(p identity.Principal) bool {
	return p.TokenID != "" && (p.Kind == "agent" || p.Kind == "session")
}

// scopeHeld reports whether held already authorizes scope: the exact string, or
// the same capability under another name or through a containing scope
// (identity.ScopeGranted). It never answers true for a scope held may not use.
func scopeHeld(held []string, scope string) bool {
	return slices.Contains(held, scope) || identity.ScopeGranted(held, scope)
}

// retainHeld returns the entries of scopes that held authorizes, in order. It
// returns a new slice (nil when nothing is held) and never adds an entry.
func retainHeld(scopes, held []string) []string {
	var out []string
	for _, scope := range scopes {
		if scopeHeld(held, scope) {
			out = append(out, scope)
		}
	}
	return out
}

func (s *Service) mintSessionCredential(ctx context.Context, sessionID string) (string, error) {
	floor, replacement, err := s.Store.ReplacementCredentialFloor(ctx, sessionID)
	if err != nil {
		return "", fmt.Errorf("%w: credential ceiling lookup: %w", store.ErrSessionReplacementUnavailable, err)
	}
	mode := string(identity.Observe)
	if s.cat() != nil {
		mode = s.cat().Global.Identity.EffectiveMode()
	}
	if replacement && floor.Mode != mode {
		return "", store.ErrSessionReplacementUnavailable
	}
	if mode == string(identity.Off) {
		return "", nil
	}
	expires := time.Now().UTC().Add(sessionTokenTTL)
	granted := slices.Clone(workerScopes)
	if s.TeamsEnabled {
		granted = append(granted, "team.write") // mcpadapter.ScopeTeamWrite; only when teams run
	}
	p := identity.Principal{ExpiresAt: &expires, ID: "msg://session/local/" + sessionID, Kind: "session", SessionID: sessionID, Display: "Session " + sessionID,
		Scopes: slices.Clone(granted), Addresses: []string{"msg://session/local/" + sessionID}}
	// The session's own logical agent is another address it may send and read
	// as. Derived from the session row, never from a request.
	if row, err := s.Store.GetSession(sessionID); err == nil && row != nil && row.LogicalAgentID != "" {
		agent := registry.LogicalAgentBindingTarget(row.LogicalAgentID)
		if addr, err := messaging.ParseURN(agent); err == nil && addr.URN() == agent {
			p.Addresses = append(p.Addresses, agent)
		}
	}
	if parent, ok := identity.FromContext(ctx); ok {
		p.CreatedBy = parent.ID
	}
	ceiling, attribution, err := s.launchCeiling(ctx, granted)
	if err != nil {
		return "", err
	}
	if err = s.refuseUnboundedWhenEnforced(attribution); err != nil {
		return "", err
	}
	p.Scopes, p.LaunchAttribution = ceiling, string(attribution)
	if replacement {
		if !slices.Contains(floor.Scopes, "*") {
			p.Scopes = slices.DeleteFunc(p.Scopes, func(scope string) bool { return !scopeHeld(floor.Scopes, scope) })
		}
		if floor.ExpiresAt != nil && floor.ExpiresAt.Before(expires) {
			p.ExpiresAt = floor.ExpiresAt
		}
		if p.Scopes == nil {
			p.Scopes = []string{}
		}
	}
	plan, err := s.Store.GetLaunchPlan(sessionID)
	if err != nil {
		return "", err
	}
	if plan.NativeResumeOnly {
		ceiling, err := s.nativeOnlyCredentialCeiling(ctx, plan)
		if err != nil {
			return "", err
		}
		if !slices.Contains(ceiling.scopes, "*") {
			p.Scopes = slices.DeleteFunc(p.Scopes, func(scope string) bool { return !scopeHeld(ceiling.scopes, scope) })
		}
		if ceiling.expires != nil && ceiling.expires.Before(*p.ExpiresAt) {
			p.ExpiresAt = ceiling.expires
		}
		if p.Scopes == nil {
			p.Scopes = []string{}
		}
	}
	token, err := identity.NewStore(s.Store.DB()).MintSessionForLaunch(ctx, p)
	if err != nil {
		if replacement {
			return "", fmt.Errorf("%w: mint inherited execution credential: %w", store.ErrSessionReplacementUnavailable, err)
		}
		return "", fmt.Errorf("mint session credential: %w", err)
	}
	s.auditLaunchAttribution(ctx, sessionID, mode, attribution)
	return token, nil
}

// sessionLaunchGate serializes preparation and minting for one session, including
// observe-mode launches that cannot mint. Entries disappear after the last waiter.
type sessionLaunchGate struct {
	slot chan struct{}
	refs int
	// holder is fresh for each acquisition, even while waiters retain this
	// same gate pointer. launchMu guards publication and invalidation.
	holder string
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
		s.launchMu.Lock()
		gate.holder = uuid.NewString()
		s.launchMu.Unlock()
		return func() {
			s.launchMu.Lock()
			gate.holder = ""
			<-gate.slot
			gate.refs--
			if gate.refs == 0 {
				delete(s.launches, id)
			}
			s.launchMu.Unlock()
		}, nil
	case <-ctx.Done():
		dropRef()
		return nil, ctx.Err()
	}
}
