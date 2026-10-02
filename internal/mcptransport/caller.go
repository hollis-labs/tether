// Package mcptransport composes credential-bound views and daemon MCP
// transports. The daemon mounts its injected handler; it does not import this
// package (the adapter's daemon client would otherwise create an import cycle).
package mcptransport

import (
	"context"
	"fmt"
	"sort"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/mcpgateway"
)

// Caller is derived exclusively from verified identity and daemon-owned state.
// Request selectors may further restrict this grant, never replace it.
type Caller struct {
	Principal identity.Principal
	Policy    mcpgateway.SessionPolicy
}

type CallerResolver struct {
	Catalog func(context.Context) (*config.Catalog, error)
	Session func(context.Context, string) (mcpgateway.SessionPolicy, error)
}

func (r CallerResolver) Resolve(ctx context.Context) (Caller, error) {
	p, verified := identity.FromContext(ctx)
	if !verified || p.ID == "" {
		return Caller{}, identity.ErrInvalidToken
	}
	if r.Catalog == nil {
		return Caller{}, fmt.Errorf("daemon MCP catalog unavailable")
	}
	cat, err := r.Catalog(ctx)
	if err != nil {
		return Caller{}, err
	}
	if cat == nil {
		return Caller{}, fmt.Errorf("daemon MCP catalog unavailable")
	}
	policy := mcpgateway.SessionPolicy{}
	switch p.Kind {
	case "session":
		if p.SessionID == "" || r.Session == nil {
			return Caller{}, fmt.Errorf("session MCP policy unavailable; resume or relaunch the session")
		}
		policy, err = r.Session(ctx, p.SessionID)
		if err != nil {
			return Caller{}, err
		}
		if err := policy.Validate(); err != nil {
			return Caller{}, err
		}
		if policy.SessionID != p.SessionID {
			return Caller{}, fmt.Errorf("session MCP policy identity mismatch")
		}
	case "operator":
		if p.ID != identity.OperatorID {
			return Caller{}, identity.ErrInvalidToken
		}
		policy.Servers = []string{}
		for id, enabled := range cat.MCPServerEnabled {
			if enabled {
				policy.Servers = append(policy.Servers, id)
			}
		}
		sort.Strings(policy.Servers)
	case "service", "interactive":
		policy.Servers, err = cat.PrincipalMCPServers(p.ID)
		if err != nil {
			return Caller{}, err
		}
	default:
		return Caller{}, identity.ErrInvalidToken
	}
	if err := cat.ValidateMCPGrant(fmt.Sprintf("verified principal %q MCP grant", p.ID), policy.Servers); err != nil {
		return Caller{}, err
	}
	// Own the returned slices/pointers; transport views must not alias mutable
	// catalog/store results after the credential binding is established.
	if policy.Profile != nil {
		value := *policy.Profile
		policy.Profile = &value
	}
	if policy.DiscoveryMode != nil {
		value := *policy.DiscoveryMode
		policy.DiscoveryMode = &value
	}
	return Caller{Principal: p, Policy: policy.Seal()}, nil
}
