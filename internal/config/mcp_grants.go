package config

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// ErrInvalidMCPGrant identifies a declared upstream that cannot be granted.
var ErrInvalidMCPGrant = errors.New("invalid MCP grant")

// PrincipalMCPServers returns only this verified principal's operator-provisioned
// grant. Missing/null grants are explicit zero authority. Callers must obtain
// principalID from identity middleware, never a request selector.
func (c *Catalog) PrincipalMCPServers(principalID string) ([]string, error) {
	grant := c.Global.Identity.MCPGrants[principalID]
	if err := c.ValidateMCPGrant(fmt.Sprintf("identity principal %q mcp_grants.servers", principalID), grant.Servers); err != nil {
		return nil, err
	}
	return append([]string{}, grant.Servers...), nil
}

// PrincipalGrantAgent returns the catalog agent id this verified principal's
// grant is bound to, or "" when the grant names none or names an agent the
// catalog does not have. Callers must obtain principalID from identity
// middleware, never a request selector.
func (c *Catalog) PrincipalGrantAgent(principalID string) string {
	id := c.Global.Identity.MCPGrants[principalID].Agent
	if _, ok := c.Agents[id]; !ok {
		return ""
	}
	return id
}

// ValidateMCPGrant checks names without spawning upstreams or resolving secrets.
func (c *Catalog) ValidateMCPGrant(owner string, ids []string) error {
	var issues []error
	for _, id := range ids {
		if id == MCPGrantAll {
			continue // every enabled upstream, expanded at launch (ExpandMCPGrant)
		}
		enabled, exists := c.MCPServerEnabled[id]
		reason := "unknown upstream (names are case-sensitive)"
		if exists && enabled {
			continue
		}
		if exists {
			reason = "disabled upstream"
		}
		issues = append(issues, fmt.Errorf("%w: %s names %q: %s", ErrInvalidMCPGrant, owner, id, reason))
	}
	return errors.Join(issues...)
}

// ValidateMCPGrantEnv checks an explicit comma-separated environment grant.
// An empty value deliberately grants no upstreams.
func (c *Catalog) ValidateMCPGrantEnv(owner, value string) error {
	if value == "" {
		return nil
	}
	ids := strings.Split(value, ",")
	for i := range ids {
		ids[i] = strings.TrimSpace(ids[i])
		// An effective grant is always expanded; a literal "*" here would grant
		// nothing downstream (every matcher is exact-name), so refuse it loudly.
		if ids[i] == MCPGrantAll {
			return fmt.Errorf("%w: %s names %q: \"*\" must be expanded to the enabled upstreams before launch", ErrInvalidMCPGrant, owner, MCPGrantAll)
		}
	}
	return c.ValidateMCPGrant(owner, ids)
}

// MCPGrantAll in an mcp.servers list means every enabled upstream
// (CW-20261114-0038), so a seat's grant follows the catalog as upstreams are added.
const MCPGrantAll = "*"

// ExpandMCPGrant replaces "*" with every enabled upstream, sorted, keeping any
// other listed ids and dropping duplicates. exclude, when set, drops upstreams
// the launching agent may not hold (machine-changing ones without
// machine_authority), so "all" never makes a launch fail on its own. A list
// without "*" is returned unchanged.
func (c *Catalog) ExpandMCPGrant(ids []string, exclude func(string) bool) []string {
	all := false
	for _, id := range ids {
		all = all || id == MCPGrantAll
	}
	if !all {
		return ids
	}
	seen := map[string]bool{}
	out := []string{}
	add := func(id string) {
		if id == MCPGrantAll || seen[id] {
			return
		}
		seen[id] = true
		out = append(out, id)
	}
	for _, id := range ids {
		add(id)
	}
	enabled := make([]string, 0, len(c.MCPServerEnabled))
	for id, on := range c.MCPServerEnabled {
		if on && (exclude == nil || !exclude(id)) {
			enabled = append(enabled, id)
		}
	}
	sort.Strings(enabled)
	for _, id := range enabled {
		add(id)
	}
	return out
}

// ValidateMCPGrants checks every declared grant, including ones currently
// shadowed by a higher-precedence owner. Implicit defaults are not declarations.
func (c *Catalog) ValidateMCPGrants() error {
	var issues []error
	for id, grant := range c.Global.Identity.MCPGrants {
		issues = append(issues, c.ValidateMCPGrant(fmt.Sprintf("identity principal %q mcp_grants.servers", id), grant.Servers))
		if _, ok := c.Agents[grant.Agent]; grant.Agent != "" && !ok {
			issues = append(issues, fmt.Errorf("%w: identity principal %q mcp_grants.agent names %q: unknown agent", ErrInvalidMCPGrant, id, grant.Agent))
		}
	}
	for id, p := range c.Projects {
		issues = append(issues, c.ValidateMCPGrant(fmt.Sprintf("project %q mcp.servers", id), p.MCP.Servers))
	}
	for id, l := range c.Launches {
		issues = append(issues, c.ValidateMCPGrant(fmt.Sprintf("launch %q mcp.servers", id), l.MCP.Servers))
		if value, set := l.Overrides.Env["TETHER_MCP_SERVERS"]; set {
			issues = append(issues, c.ValidateMCPGrantEnv(fmt.Sprintf("launch %q overrides.env.TETHER_MCP_SERVERS", id), value))
		}
	}
	for id, a := range c.Agents {
		issues = append(issues, c.ValidateMCPGrant(fmt.Sprintf("agent %q mcp.servers", id), a.MCP.Servers))
		if value, set := a.Env["TETHER_MCP_SERVERS"]; set {
			issues = append(issues, c.ValidateMCPGrantEnv(fmt.Sprintf("agent %q env.TETHER_MCP_SERVERS", id), value))
		}
		for provider, override := range a.ProviderOverrides {
			if value, set := override.Env["TETHER_MCP_SERVERS"]; set {
				issues = append(issues, c.ValidateMCPGrantEnv(fmt.Sprintf("agent %q provider %q env.TETHER_MCP_SERVERS", id, provider), value))
			}
		}
	}
	for id, ids := range c.BootMCPGrants {
		issues = append(issues, c.ValidateMCPGrant(fmt.Sprintf("boot profile %q mcp_servers", id), ids))
	}
	var messages []string
	for _, issue := range issues {
		if issue != nil {
			messages = append(messages, issue.Error())
		}
	}
	sort.Strings(messages)
	if len(messages) == 0 {
		return nil
	}
	return fmt.Errorf("%w:\n%s", ErrInvalidMCPGrant, strings.Join(messages, "\n"))
}

// ValidateLaunchMCPGrants checks only owners in this launch's selected chain.
// Startup and doctor use ValidateMCPGrants to report all catalog declarations.
func (c *Catalog) ValidateLaunchMCPGrants(id string) error {
	l, exists := c.Launches[id]
	if !exists {
		return nil
	} // normal launch-reference validation reports this
	p := c.Projects[l.Project]
	a := c.Agents[l.Agent]
	var issues []error
	issues = append(issues, c.ValidateMCPGrant(fmt.Sprintf("agent %q mcp.servers", l.Agent), a.MCP.Servers))
	issues = append(issues, c.ValidateMCPGrant(fmt.Sprintf("launch %q mcp.servers", id), l.MCP.Servers))
	issues = append(issues, c.ValidateMCPGrant(fmt.Sprintf("project %q mcp.servers", l.Project), p.MCP.Servers))
	for owner, env := range map[string]map[string]string{
		fmt.Sprintf("launch %q overrides.env.TETHER_MCP_SERVERS", id):                   l.Overrides.Env,
		fmt.Sprintf("agent %q env.TETHER_MCP_SERVERS", l.Agent):                         a.Env,
		fmt.Sprintf("agent %q provider %q env.TETHER_MCP_SERVERS", l.Agent, l.Provider): a.ProviderOverrides[l.Provider].Env,
	} {
		if value, set := env["TETHER_MCP_SERVERS"]; set {
			issues = append(issues, c.ValidateMCPGrantEnv(owner, value))
		}
	}
	return errors.Join(issues...)
}
